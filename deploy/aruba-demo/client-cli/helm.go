package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// helmStack drives the Helm v4 Go library against one kubeconfig. Charts are
// resolved straight from their repository URL (LocateChart with RepoURL), so
// there is no `helm repo add` step and nothing is written to the user's Helm
// repository config.
type helmStack struct{ kubeconfig string }

func (h *helmStack) config(ns string) (*action.Configuration, *cli.EnvSettings, error) {
	settings := cli.New()
	settings.KubeConfig = h.kubeconfig
	settings.SetNamespace(ns)
	cfg := new(action.Configuration)
	if err := cfg.Init(settings.RESTClientGetter(), ns, "secret"); err != nil {
		return nil, nil, fmt.Errorf("helm init for namespace %s: %w", ns, err)
	}
	return cfg, settings, nil
}

type chartRef struct {
	repoURL string
	name    string
	version string
}

// loadChart resolves and downloads a chart from its repository URL.
func (h *helmStack) loadChart(ns string, ref chartRef) (chart.Charter, error) {
	cfg, settings, err := h.config(ns)
	if err != nil {
		return nil, err
	}
	cp := action.NewInstall(cfg)
	cp.RepoURL = ref.repoURL
	cp.Version = ref.version
	path, err := cp.LocateChart(ref.name, settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart %s %s: %w", ref.name, ref.version, err)
	}
	return loader.Load(path)
}

// crdObjects returns the raw YAML of every file under the chart's crds/ (and its
// dependencies'). Helm installs those only on a first install and never upgrades
// them, so an ECP version bump needs them applied by hand.
func crdObjects(c chart.Charter) [][]byte {
	ch, ok := c.(*chartv2.Chart)
	if !ok {
		return nil
	}
	var out [][]byte
	for _, crd := range ch.CRDObjects() {
		out = append(out, crd.File.Data)
	}
	return out
}

type installOpts struct {
	wait           bool
	timeout        time.Duration
	forceConflicts bool
}

// upgradeInstall is `helm upgrade --install`: install when the release is absent,
// upgrade when it is there. Both are server-side applies, as Helm 4 does by default.
func (h *helmStack) upgradeInstall(ctx context.Context, ns, release string, ref chartRef, vals map[string]any, o installOpts) error {
	cfg, settings, err := h.config(ns)
	if err != nil {
		return err
	}

	exists := true
	hist := action.NewHistory(cfg)
	hist.Max = 1
	if _, err := hist.Run(release); errors.Is(err, driver.ErrReleaseNotFound) {
		exists = false
	} else if err != nil {
		return fmt.Errorf("history %s: %w", release, err)
	}

	waitStrategy := kube.HookOnlyStrategy
	if o.wait {
		waitStrategy = kube.StatusWatcherStrategy
	}

	if !exists {
		act := action.NewInstall(cfg)
		act.RepoURL, act.Version = ref.repoURL, ref.version
		act.ReleaseName, act.Namespace = release, ns
		act.CreateNamespace = true
		act.WaitStrategy, act.Timeout = waitStrategy, o.timeout
		act.ForceConflicts = o.forceConflicts
		path, err := act.LocateChart(ref.name, settings)
		if err != nil {
			return fmt.Errorf("locate chart %s %s: %w", ref.name, ref.version, err)
		}
		ch, err := loader.Load(path)
		if err != nil {
			return err
		}
		_, err = act.RunWithContext(ctx, ch, vals)
		return err
	}

	act := action.NewUpgrade(cfg)
	act.RepoURL, act.Version = ref.repoURL, ref.version
	act.Namespace = ns
	act.Install = true
	act.WaitStrategy, act.Timeout = waitStrategy, o.timeout
	act.ForceConflicts = o.forceConflicts
	path, err := act.LocateChart(ref.name, settings)
	if err != nil {
		return fmt.Errorf("locate chart %s %s: %w", ref.name, ref.version, err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return err
	}
	_, err = act.RunWithContext(ctx, release, ch, vals)
	return err
}

func (h *helmStack) uninstall(ns, release string, timeout time.Duration) error {
	cfg, _, err := h.config(ns)
	if err != nil {
		return err
	}
	act := action.NewUninstall(cfg)
	act.Timeout = timeout
	act.IgnoreNotFound = true
	// Not optional: Uninstall refuses to run with an unset strategy.
	act.WaitStrategy = kube.StatusWatcherStrategy
	_, err = act.Run(release)
	return err
}

type releaseInfo struct {
	Name      string
	Namespace string
	Chart     string
	Version   string
	App       string
	Status    string
	Revision  int
}

func (h *helmStack) list(ns string) ([]releaseInfo, error) {
	cfg, _, err := h.config(ns)
	if err != nil {
		return nil, err
	}
	act := action.NewList(cfg)
	act.All = true
	rels, err := act.Run()
	if err != nil {
		return nil, err
	}
	out := make([]releaseInfo, 0, len(rels))
	for _, r := range rels {
		rel, ok := r.(*releasev1.Release)
		if !ok {
			continue
		}
		info := releaseInfo{
			Name: rel.Name, Namespace: rel.Namespace, Revision: rel.Version,
			Status: rel.Info.Status.String(),
		}
		if ch := rel.Chart; ch != nil && ch.Metadata != nil {
			info.Chart, info.Version, info.App = ch.Metadata.Name, ch.Metadata.Version, ch.Metadata.AppVersion
		}
		out = append(out, info)
	}
	return out, nil
}
