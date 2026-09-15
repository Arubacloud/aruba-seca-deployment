package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// fieldManager owns every field this CLI server-side applies. Keeping it stable
// is what makes a re-run an update rather than a conflict.
const fieldManager = "aruba-demo-cli"

// kube bundles the clients every cluster-side step needs, all built off one
// kubeconfig file — the one downloaded from the KaaS cluster.
type kubeClient struct {
	clientset *kubernetes.Clientset
	dynamic   dynamic.Interface
	disco     discovery.DiscoveryInterface
}

func newKube(kubeconfig string) (*kubeClient, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubeClient{clientset: cs, dynamic: dyn, disco: cs.Discovery()}, nil
}

// apply server-side applies every document in a multi-document YAML stream.
//
// The mapper is rebuilt on each call, and a no-match is retried: CRDs installed
// moments earlier are not in discovery yet, so the first apply of a CR right
// after its CRD would otherwise fail on a stale mapper.
func (k *kubeClient) apply(ctx context.Context, doc []byte, defaultNS string) error {
	objs, err := decodeYAML(doc)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := k.applyOne(ctx, obj, defaultNS); err != nil {
			return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

func (k *kubeClient) applyOne(ctx context.Context, obj *unstructured.Unstructured, defaultNS string) error {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	force := true
	deadline := time.Now().Add(90 * time.Second)
	for {
		mapping, err := k.restMapping(obj)
		if err == nil {
			var ri dynamic.ResourceInterface = k.dynamic.Resource(mapping.Resource)
			if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
				ns := obj.GetNamespace()
				if ns == "" {
					ns = defaultNS
				}
				ri = k.dynamic.Resource(mapping.Resource).Namespace(ns)
			}
			_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
				metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
		}
		// A CRD applied seconds ago is not in discovery yet; so is a CR whose
		// CRD is still being established. Both settle on their own.
		if err == nil || !(meta.IsNoMatchError(err) || apierrors.IsNotFound(err)) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}

func (k *kubeClient) restMapping(obj *unstructured.Unstructured) (*meta.RESTMapping, error) {
	groups, err := restmapper.GetAPIGroupResources(k.disco)
	if err != nil {
		return nil, err
	}
	gvk := obj.GroupVersionKind()
	return restmapper.NewDiscoveryRESTMapper(groups).RESTMapping(gvk.GroupKind(), gvk.Version)
}

func decodeYAML(doc []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		if len(obj.Object) == 0 { // a --- separator with only comments after it
			continue
		}
		out = append(out, obj)
	}
}

func (k *kubeClient) ensureNamespace(ctx context.Context, name string) error {
	_, err := k.clientset.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ensurePullSecret writes a docker-registry Secret. The ghcr.io/eu-sovereign-cloud
// ECP packages are private, so without one every ECP pod sits in ImagePullBackOff.
// The credential is a short-lived gh token, so this overwrites rather than skips.
func (k *kubeClient) ensurePullSecret(ctx context.Context, ns, name, server, user, token string) error {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	cfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{server: map[string]string{
			"username": user, "password": token, "auth": auth,
		}},
	})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: cfg},
	}
	secrets := k.clientset.CoreV1().Secrets(ns)
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
		return err
	} else if err != nil {
		return err
	}
	return nil
}

// restartDeployment is `kubectl rollout restart`: the gateways read the JWT key
// file once at startup and the chart puts no checksum annotation on the pod
// template, so a rotated key changes nothing until the pods are replaced.
func (k *kubeClient) restartDeployment(ctx context.Context, ns, name string) error {
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339))
	_, err := k.clientset.AppsV1().Deployments(ns).Patch(ctx, name,
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// waitDeployment is `kubectl rollout status`.
func (k *kubeClient) waitDeployment(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		d, err := k.clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if rolloutComplete(d) {
				return nil
			}
			last = fmt.Sprintf("%d/%d updated, %d available",
				d.Status.UpdatedReplicas, want(d), d.Status.AvailableReplicas)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s not ready after %s: %s", ns, name, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func want(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

func rolloutComplete(d *appsv1.Deployment) bool {
	return d.Generation == d.Status.ObservedGeneration &&
		d.Status.UpdatedReplicas == want(d) &&
		d.Status.Replicas == d.Status.UpdatedReplicas &&
		d.Status.AvailableReplicas == d.Status.UpdatedReplicas
}

// deploymentImages reports the container images of every Deployment in a
// namespace — the honest answer to "which version is actually running", as
// opposed to which chart version Helm recorded.
func (k *kubeClient) deploymentImages(ctx context.Context, ns string) (map[string][]string, error) {
	list, err := k.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, d := range list.Items {
		for _, c := range d.Spec.Template.Spec.Containers {
			out[d.Name] = append(out[d.Name], c.Image)
		}
	}
	return out, nil
}
