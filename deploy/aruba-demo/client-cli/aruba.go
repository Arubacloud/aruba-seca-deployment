package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Arubacloud/sdk-go/pkg/aruba"
)

// demoTags is stamped on everything this CLI creates, so the demo's resources can
// be told apart from the account's own in the Aruba console.
//
// The Aruba API validates every tag against `^[A-Za-z0-9-]{4,30}$` and the SDK does
// not check client-side, so a tag shorter than FOUR characters is only rejected once
// the request lands — as a bare "One or more validation errors occurred." with no
// field named. "ecp" is three. See TestDemoTagsAreValid.
var demoTags = []string{"ecp-demo", "seca"}

// KaaS clusters take 10-20 minutes to converge, well past the SDK's 600s default.
var kaasWait = []aruba.WaitOption{
	aruba.WithTimeout(40 * time.Minute),
	aruba.WithRetries(240),
	aruba.WithBaseDelay(10 * time.Second),
}

// clusterSpec is everything the demo cluster needs on the Aruba side. Every name
// is looked up before it is created, so a re-run adopts what is already there
// rather than billing a second copy.
type clusterSpec struct {
	project     string
	name        string
	region      aruba.Region
	zone        aruba.Zone
	k8sVersion  aruba.KubernetesVersion
	nodeFlavor  aruba.NodePoolInstance
	nodeCount   int
	subnetCIDR  string
	podCIDR     string
	nodeCIDR    string
	highlyAvail bool
}

func newArubaClient(clientID, clientSecret string) (aruba.Client, error) {
	return aruba.NewClient(aruba.DefaultOptions(clientID, clientSecret).WithNoLogs())
}

// arubaErr adds the field-level validation errors to an Aruba API error.
//
// *aruba.HTTPError.Error() prints only the RFC 7807 title and detail, so a 400
// arrives as the useless "HTTP 400: One or more validation errors occurred." while
// the body's `errors` array — which names the offending field — is dropped on the
// floor. Every call site in this file wraps with this instead.
func arubaErr(err error) error {
	var httpErr *aruba.HTTPError
	if !errors.As(err, &httpErr) || httpErr.ErrResp == nil {
		return err
	}
	var fields []string
	for _, v := range httpErr.ErrResp.Errors {
		fields = append(fields, strings.TrimSpace(v.Field+": "+v.Message))
	}
	if len(fields) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s)", err, strings.Join(fields, "; "))
}

// ensureCluster provisions Project -> VPC -> Subnet -> KaaS, in that order,
// waiting for each to settle before the next depends on it.
func ensureCluster(ctx context.Context, c aruba.Client, s clusterSpec) (*aruba.KaaS, error) {
	proj, err := ensureProject(ctx, c, s.project)
	if err != nil {
		return nil, err
	}
	logf("project %s (%s)", proj.Name(), proj.ID())

	vpc, err := ensureVPC(ctx, c, proj, s)
	if err != nil {
		return nil, err
	}
	logf("vpc %s (%s)", vpc.Name(), vpc.ID())

	subnet, err := ensureSubnet(ctx, c, vpc, s)
	if err != nil {
		return nil, err
	}
	logf("subnet %s (%s) %s", subnet.Name(), subnet.ID(), s.subnetCIDR)

	return ensureKaaS(ctx, c, proj, vpc, subnet, s)
}

func ensureProject(ctx context.Context, c aruba.Client, name string) (*aruba.Project, error) {
	list, err := c.FromProject().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", arubaErr(err))
	}
	for _, p := range list.Items() {
		if p.Name() == name {
			return p, nil
		}
	}
	p, err := c.FromProject().Create(ctx, aruba.NewProject().
		Named(name).
		Tagged(demoTags...).
		DescribedAs("ECP / SECA demo stack"))
	if err != nil {
		return nil, fmt.Errorf("create project %s: %w", name, arubaErr(err))
	}
	return p, nil
}

func ensureVPC(ctx context.Context, c aruba.Client, proj aruba.Ref, s clusterSpec) (*aruba.VPC, error) {
	name := s.name + "-vpc"
	list, err := c.FromNetwork().VPCs().List(ctx, proj)
	if err != nil {
		return nil, fmt.Errorf("list VPCs: %w", arubaErr(err))
	}
	for _, v := range list.Items() {
		if v.Name() == name {
			return v, v.WaitUntilActive(ctx)
		}
	}
	v, err := c.FromNetwork().VPCs().Create(ctx, aruba.NewVPC().
		Named(name).
		Tagged(demoTags...).
		InProject(proj).
		InRegion(s.region).
		WithoutPreset())
	if err != nil {
		return nil, fmt.Errorf("create VPC %s: %w", name, arubaErr(err))
	}
	if err := v.WaitUntilReady(ctx); err != nil {
		return nil, fmt.Errorf("VPC %s never became ready: %w", name, err)
	}
	return v, nil
}

func ensureSubnet(ctx context.Context, c aruba.Client, vpc *aruba.VPC, s clusterSpec) (*aruba.Subnet, error) {
	name := s.name + "-subnet"
	list, err := c.FromNetwork().Subnets().List(ctx, vpc)
	if err != nil {
		return nil, fmt.Errorf("list subnets: %w", arubaErr(err))
	}
	for _, sn := range list.Items() {
		if sn.Name() == name {
			return sn, sn.WaitUntilActive(ctx)
		}
	}
	sn, err := c.FromNetwork().Subnets().Create(ctx, aruba.NewSubnet().
		OfType(aruba.SubnetTypeAdvanced).
		Named(name).
		Tagged(demoTags...).
		InVPC(vpc).
		InRegion(s.region).
		WithCIDR(s.subnetCIDR))
	if err != nil {
		return nil, fmt.Errorf("create subnet %s: %w", name, arubaErr(err))
	}
	if err := sn.WaitUntilReady(ctx); err != nil {
		return nil, fmt.Errorf("subnet %s never became ready: %w", name, err)
	}
	return sn, nil
}

func ensureKaaS(ctx context.Context, c aruba.Client, proj aruba.Ref, vpc *aruba.VPC, subnet *aruba.Subnet, s clusterSpec) (*aruba.KaaS, error) {
	if existing, err := findKaaS(ctx, c, proj, s.name); err != nil {
		return nil, err
	} else if existing != nil {
		logf("kaas %s already exists (%s), waiting for it to be ready", existing.Name(), existing.State())
		if err := existing.WaitUntilReady(ctx, kaasWait...); err != nil {
			return nil, err
		}
		return existing, nil
	}

	// The KaaS API stores only the security group's NAME, not a URI, and creates
	// the group itself — so no SecurityGroup resource has to exist beforehand.
	k := aruba.NewKaaS().
		Named(s.name).
		Tagged(demoTags...).
		InProject(proj).
		InRegion(s.region).
		WithKubernetesVersion(s.k8sVersion).
		WithPodCIDR(s.podCIDR).
		WithNodeCIDR(s.nodeCIDR, s.name+"-nodes").
		WithVPC(vpc).
		WithSubnet(subnet).
		WithSecurityGroupName(s.name + "-sg").
		WithNodePools(aruba.NewNodePool().
			Named(s.name + "-pool").
			OfInstance(s.nodeFlavor).
			InZone(s.zone).
			WithCount(s.nodeCount)).
		BilledBy(aruba.BillingPeriodHour)
	if s.highlyAvail {
		k = k.HighlyAvailable()
	}

	created, err := c.FromContainer().KaaS().Create(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("create KaaS %s: %w", s.name, arubaErr(err))
	}
	logf("kaas %s submitted (%s) — this takes 10-20 minutes", created.Name(), created.KaaSID())
	if err := created.WaitUntilReady(ctx, kaasWait...); err != nil {
		return nil, fmt.Errorf("KaaS %s never became ready: %w", s.name, err)
	}
	return created, nil
}

func findKaaS(ctx context.Context, c aruba.Client, proj aruba.Ref, name string) (*aruba.KaaS, error) {
	list, err := c.FromContainer().KaaS().List(ctx, proj)
	if err != nil {
		return nil, fmt.Errorf("list KaaS clusters: %w", arubaErr(err))
	}
	for _, k := range list.Items() {
		if k.Name() == name {
			return k, nil
		}
	}
	return nil, nil
}

// writeKubeconfig downloads the cluster's kubeconfig and writes it 0600. Every
// later step in this CLI talks to the cluster through this file alone, so it
// never reads or writes the caller's ambient kube context.
func writeKubeconfig(ctx context.Context, k *aruba.KaaS, path string) error {
	raw, err := k.DownloadKubeconfig(ctx)
	if err != nil {
		return fmt.Errorf("download kubeconfig: %w", arubaErr(err))
	}
	if len(raw) == 0 {
		return fmt.Errorf("the cluster returned an empty kubeconfig")
	}
	decodedRaw, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return fmt.Errorf("decode kubeconfig: %w", err)
	}
	if err := os.WriteFile(path, decodedRaw, 0o600); err != nil {
		return err
	}
	logf("kubeconfig written to %s", path)
	return nil
}

// deleteCluster tears the Aruba side down in reverse dependency order. It does
// NOT delete the project: it may be shared, and Aruba refuses a non-empty one.
func deleteCluster(ctx context.Context, c aruba.Client, s clusterSpec) error {
	proj, err := findProject(ctx, c, s.project)
	if err != nil || proj == nil {
		return err
	}

	if k, err := findKaaS(ctx, c, proj, s.name); err != nil {
		return err
	} else if k != nil {
		logf("deleting kaas %s", k.Name())
		if err := c.FromContainer().KaaS().Delete(ctx, k); err != nil {
			return fmt.Errorf("delete KaaS: %w", arubaErr(err))
		}
		if err := k.WaitUntilGone(ctx, kaasWait...); err != nil {
			return fmt.Errorf("KaaS not gone: %w", err)
		}
	}

	vpcs, err := c.FromNetwork().VPCs().List(ctx, proj)
	if err != nil {
		return err
	}
	for _, v := range vpcs.Items() {
		if v.Name() != s.name+"-vpc" {
			continue
		}
		subnets, err := c.FromNetwork().Subnets().List(ctx, v)
		if err != nil {
			return err
		}
		for _, sn := range subnets.Items() {
			logf("deleting subnet %s", sn.Name())
			if err := c.FromNetwork().Subnets().Delete(ctx, sn); err != nil {
				return fmt.Errorf("delete subnet %s: %w", sn.Name(), arubaErr(err))
			}
			if err := sn.WaitUntilGone(ctx); err != nil {
				return fmt.Errorf("subnet %s not gone: %w", sn.Name(), err)
			}
		}
		logf("deleting vpc %s", v.Name())
		if err := c.FromNetwork().VPCs().Delete(ctx, v); err != nil {
			return fmt.Errorf("delete VPC %s: %w", v.Name(), arubaErr(err))
		}
		if err := v.WaitUntilGone(ctx); err != nil {
			return fmt.Errorf("VPC %s not gone: %w", v.Name(), err)
		}
	}
	return nil
}

func findProject(ctx context.Context, c aruba.Client, name string) (*aruba.Project, error) {
	list, err := c.FromProject().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", arubaErr(err))
	}
	for _, p := range list.Items() {
		if p.Name() == name {
			return p, nil
		}
	}
	return nil, nil
}
