// Command aruba-demo is the Go replacement for deploy/aruba-demo/install.sh and
// seed-tenant.sh, extended with the one thing the scripts could not do: creating
// the Kubernetes cluster itself, as an Aruba KaaS cluster, instead of assuming a
// kind or a hand-built one.
//
// It talks to exactly two things: the Aruba Cloud API (github.com/Arubacloud/sdk-go)
// and one Kubernetes cluster through a kubeconfig FILE it downloads itself. It never
// reads or switches the caller's ambient kube context.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Arubacloud/sdk-go/pkg/aruba"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type config struct {
	// Aruba API client. One credential drives both halves: the operator
	// authenticates to the CMP with it, and Terraform mints its bearer token from it.
	clientID     string
	clientSecret string
	envFile      string

	// SECA identity. tenant is NOT needed by the operator in single-tenant mode
	// (see README) but it is still the SECA tenant: it keys the API path, hashes
	// to the tenant namespace, and scopes the RoleAssignment.
	tenant     string
	region     string
	location   string
	jwtSubject string

	// Aruba production endpoints.
	apiGateway string
	idp        string
	realm      string

	// The KaaS cluster.
	clusterName string
	projectName string
	k8sVersion  string
	nodeFlavor  string
	nodeCount   int
	zone        string
	subnetCIDR  string
	podCIDR     string
	nodeCIDR    string
	highlyAvail bool
	kubeconfig  string

	// The stack.
	ecpVersion      string
	operatorVersion string
	ecpNS           string
	arubaNS         string
	pullSecret      string
	ghcrUser        string
	ghcrToken       string
	regionalURL     string
	manifestsDir    string
	timeout         time.Duration

	gatewayImageGlobal   string
	gatewayImageRegional string
	gatewayImageTag      string
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "==> "+format+"\n", a...)
}

func main() {
	c := &config{}
	root := &cobra.Command{
		Use:   "aruba-demo",
		Short: "Provision an Aruba KaaS cluster and deploy the ECP/SECA demo stack onto it",
		Long: "aruba-demo builds the Aruba side of the SECA demo (Project, VPC, Subnet, KaaS cluster)\n" +
			"and installs the whole ECP stack into it: the arubacloud-resource-operator, the two ECP\n" +
			"gateways, the aruba delegator, the region catalog and the tenant's RBAC.\n\n" +
			"It is the Go replacement for install.sh + seed-tenant.sh. Every command is idempotent.\n\n" +
			"WARNING: this provisions real, billable Aruba resources.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Credentials and identity are needed by nearly every command, so they are
	// persistent flags on the root rather than repeated on each.
	pf := root.PersistentFlags()
	pf.StringVar(&c.clientID, "client-id", "", "Aruba API client id (env ARUBA_CLIENT_ID, .env, or prompted)")
	pf.StringVar(&c.clientSecret, "client-secret", "", "Aruba API client secret (env ARUBA_CLIENT_SECRET, .env, or prompted)")
	pf.StringVar(&c.envFile, "env-file", "", "read ARUBA_CLIENT_ID/ARUBA_CLIENT_SECRET from this file (default: ./.env then ../.env)")
	pf.StringVar(&c.tenant, "tenant", "ARU-348095", "the SECA tenant: keys the API path, the tenant namespace and the RoleAssignment")
	pf.StringVar(&c.region, "region", "itbg-bergamo", "name of the SECA Region CR — a Kubernetes object name, so lowercase")
	pf.StringVar(&c.location, "location", "ITBG-Bergamo", "what the regional gateway stamps on resources; Aruba rejects any other case")
	pf.StringVar(&c.apiGateway, "api-gateway", "https://api.arubacloud.com", "Aruba CMP API endpoint")
	pf.StringVar(&c.idp, "idp", "https://mylogin.aruba.it/auth", "Aruba IdP base URL")
	pf.StringVar(&c.realm, "realm", "cmp-new-apikey", "Keycloak realm that signs the API client's tokens")
	pf.StringVar(&c.clusterName, "cluster", "ecp-demo", "name of the KaaS cluster (and of its VPC, subnet and node pool)")
	pf.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file to use (default: ./<cluster>.kubeconfig)")
	pf.StringVar(&c.ecpNS, "ecp-namespace", "ecp-system", "namespace for the ECP gateways")
	pf.StringVar(&c.arubaNS, "aruba-namespace", "aruba-system", "namespace for the operator and the delegator")

	root.AddCommand(
		clusterCmd(c),
		installCmd(c),
		upCmd(c),
		uninstallCmd(c),
		versionsCmd(c),
		tenantCmd(c),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// --- cluster -----------------------------------------------------------------

func clusterCmd(c *config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage the Aruba KaaS cluster the stack runs on",
	}

	create := &cobra.Command{
		Use:   "create",
		Short: "Create (or adopt) the Project, VPC, Subnet and KaaS cluster, then write its kubeconfig",
		Long: "Every resource is looked up by name before it is created, so a re-run adopts what is\n" +
			"already there rather than billing a second copy. The KaaS cluster itself takes 10-20 minutes.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(true); err != nil {
				return err
			}
			client, err := newArubaClient(c.clientID, c.clientSecret)
			if err != nil {
				return err
			}
			spec, err := c.clusterSpec()
			if err != nil {
				return err
			}
			k, err := ensureCluster(cmd.Context(), client, spec)
			if err != nil {
				return err
			}
			return writeKubeconfig(cmd.Context(), k, c.kubeconfig)
		},
	}
	c.addClusterFlags(create)

	kubeconfig := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Re-download the cluster's kubeconfig",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(true); err != nil {
				return err
			}
			client, err := newArubaClient(c.clientID, c.clientSecret)
			if err != nil {
				return err
			}
			proj, err := findProject(cmd.Context(), client, c.projectName)
			if err != nil {
				return err
			}
			if proj == nil {
				return fmt.Errorf("no project %q", c.projectName)
			}
			k, err := findKaaS(cmd.Context(), client, proj, c.clusterName)
			if err != nil {
				return err
			}
			if k == nil {
				return fmt.Errorf("no KaaS cluster %q in project %q", c.clusterName, c.projectName)
			}
			return writeKubeconfig(cmd.Context(), k, c.kubeconfig)
		},
	}
	kubeconfig.Flags().StringVar(&c.projectName, "project", "ecp-demo", "Aruba project holding the cluster")

	del := &cobra.Command{
		Use:   "delete",
		Short: "Delete the KaaS cluster, its subnet and its VPC (the project is left alone)",
		Long: "Delete the SECA resources FIRST — from Terraform, or by deleting the CRs. Their finalizers\n" +
			"are what make the plugin delete the arubacloud.com CRs, which is what makes the operator reap\n" +
			"what it created in the CMP. Dropping the cluster first strands real, billable resources.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(true); err != nil {
				return err
			}
			if !c.confirm(cmd, "delete the KaaS cluster %q and its network in project %q", c.clusterName, c.projectName) {
				return errors.New("aborted")
			}
			client, err := newArubaClient(c.clientID, c.clientSecret)
			if err != nil {
				return err
			}
			spec, err := c.clusterSpec()
			if err != nil {
				return err
			}
			return deleteCluster(cmd.Context(), client, spec)
		},
	}
	c.addClusterFlags(del)
	del.Flags().Bool("yes", false, "do not ask for confirmation")

	cmd.AddCommand(create, kubeconfig, del)
	return cmd
}

func (c *config) addClusterFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&c.projectName, "project", "ecp-demo", "Aruba project to hold the cluster (created if absent)")
	f.StringVar(&c.k8sVersion, "k8s-version", "1.34.1", "Kubernetes version (one Aruba publishes: 1.32.3, 1.33.2, 1.34.1)")
	f.StringVar(&c.nodeFlavor, "node-flavor", "K4A8", "node pool instance flavor, e.g. K2A4 (2 vCPU/4 GB) or K4A8 (4 vCPU/8 GB)")
	f.IntVar(&c.nodeCount, "nodes", 2, "number of nodes in the pool")
	f.StringVar(&c.zone, "zone", "ITBG-1", "availability zone for the node pool")
	f.StringVar(&c.subnetCIDR, "subnet-cidr", "10.0.1.0/24", "CIDR of the cluster's subnet")
	f.StringVar(&c.podCIDR, "pod-cidr", "10.0.3.0/24", "pod CIDR")
	f.StringVar(&c.nodeCIDR, "node-cidr", "172.16.0.0/16", "node CIDR")
	f.BoolVar(&c.highlyAvail, "ha", false, "run a highly available control plane (costs more)")
}

// --- install / up ------------------------------------------------------------

func installCmd(c *config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the whole ECP stack into the cluster (the install.sh replica)",
		Long: "Installs, in order: the namespaces and the GHCR pull secret, the arubacloud-resource-operator\n" +
			"in auth.mode=single, the ECP CRDs and both gateways with JWT auth, the aruba delegator, the\n" +
			"region catalog and the tenant's SKUs and RBAC. Idempotent: re-run it to reconfigure.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(true); err != nil {
				return err
			}
			return install(cmd.Context(), c)
		},
	}
	c.addStackFlags(cmd)
	return cmd
}

func upCmd(c *config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "cluster create + install, in one shot",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(true); err != nil {
				return err
			}
			client, err := newArubaClient(c.clientID, c.clientSecret)
			if err != nil {
				return err
			}
			spec, err := c.clusterSpec()
			if err != nil {
				return err
			}
			k, err := ensureCluster(cmd.Context(), client, spec)
			if err != nil {
				return err
			}
			if err := writeKubeconfig(cmd.Context(), k, c.kubeconfig); err != nil {
				return err
			}
			return install(cmd.Context(), c)
		},
	}
	c.addClusterFlags(cmd)
	c.addStackFlags(cmd)
	return cmd
}

func (c *config) addStackFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&c.ecpVersion, "ecp-version", "0.0.3-alpha", "ecp and ecp-delegator chart version (>= 0.0.3-alpha: earlier tags cannot verify a real OIDC token)")
	f.StringVar(&c.operatorVersion, "operator-version", "1.0.1", "arubacloud-resource-operator chart version")
	f.StringVar(&c.pullSecret, "pull-secret", "ghcr-ecp", "name of the image pull Secret created in both namespaces")
	f.StringVar(&c.ghcrUser, "ghcr-user", "", "GHCR username (default: `gh api user -q .login`)")
	f.StringVar(&c.ghcrToken, "ghcr-token", "", "GHCR token with read:packages (default: `gh auth token`)")
	f.StringVar(&c.regionalURL, "regional-url", "http://127.0.0.1:3001", "what the region catalog advertises — must be reachable FROM THE CLIENT")
	f.StringVar(&c.manifestsDir, "manifests-dir", "", "read catalog.yaml/tenant.yaml from this directory instead of the embedded copies")
	f.DurationVar(&c.timeout, "timeout", 10*time.Minute, "per-Helm-release timeout")
	f.StringVar(&c.jwtSubject, "jwt-subject", "", "skip minting a token and grant this subject instead")
	f.StringVar(&c.gatewayImageGlobal, "gateway-image-global", "", "override the global gateway image repository (testing an unreleased build)")
	f.StringVar(&c.gatewayImageRegional, "gateway-image-regional", "", "override the regional gateway image repository")
	f.StringVar(&c.gatewayImageTag, "gateway-image-tag", "", "tag for both gateway image overrides; the overrides are ignored unless this is set")
}

// --- uninstall / versions / tenant -------------------------------------------

func uninstallCmd(c *config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the three Helm releases (leaves CRDs, CRs and namespaces alone)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(false); err != nil {
				return err
			}
			if !c.confirm(cmd, "uninstall ecp, ecp-delegator and arubacloud-resource-operator") {
				return errors.New("aborted")
			}
			return uninstall(cmd.Context(), c)
		},
	}
	cmd.Flags().DurationVar(&c.timeout, "timeout", 10*time.Minute, "per-release timeout")
	cmd.Flags().Bool("yes", false, "do not ask for confirmation")
	return cmd
}

func versionsCmd(c *config) *cobra.Command {
	return &cobra.Command{
		Use:     "versions",
		Aliases: []string{"status"},
		Short:   "Show the installed chart versions and the running container images",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(false); err != nil {
				return err
			}
			return versions(cmd.Context(), c)
		},
	}
}

func tenantCmd(c *config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Manage SECA tenants on a deployed stack",
	}
	add := &cobra.Command{
		Use:   "add",
		Short: "Apply one tenant's namespace, SKU catalogs, Role and RoleAssignment (the seed-tenant.sh replica)",
		Long: "Writes tenant.yaml only. It does not run Helm and does not touch the cluster-scoped Region CR,\n" +
			"so onboarding a tenant cannot disturb one that is already working.\n\n" +
			"Note that with the operator in auth.mode=single every tenant on this cluster is billed to the\n" +
			"ONE Aruba account the operator holds credentials for. Use --client-id/--client-secret only to\n" +
			"derive the subject to grant.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.resolve(c.jwtSubject == ""); err != nil {
				return err
			}
			k, err := newKube(c.kubeconfig)
			if err != nil {
				return err
			}
			subject := c.jwtSubject
			if subject == "" {
				token, err := mintToken(cmd.Context(), realmURL(c.idp, c.realm), c.clientID, c.clientSecret)
				if err != nil {
					return err
				}
				if subject, err = tokenSubject(token); err != nil {
					return err
				}
			}
			logf("token subject: %s", subject)
			return applyTenant(cmd.Context(), c, k, subject)
		},
	}
	add.Flags().StringVar(&c.jwtSubject, "jwt-subject", "", "grant this subject instead of minting a token to discover it")
	add.Flags().StringVar(&c.manifestsDir, "manifests-dir", "", "read tenant.yaml from this directory instead of the embedded copy")
	cmd.AddCommand(add)
	return cmd
}

// --- configuration resolution ------------------------------------------------

func (c *config) clusterSpec() (clusterSpec, error) {
	if c.nodeCount < 1 {
		return clusterSpec{}, fmt.Errorf("--nodes must be at least 1")
	}
	return clusterSpec{
		project:     c.projectName,
		name:        c.clusterName,
		region:      aruba.Region(c.location),
		zone:        aruba.Zone(c.zone),
		k8sVersion:  aruba.KubernetesVersion(c.k8sVersion),
		nodeFlavor:  aruba.NodePoolInstance(c.nodeFlavor),
		nodeCount:   c.nodeCount,
		subnetCIDR:  c.subnetCIDR,
		podCIDR:     c.podCIDR,
		nodeCIDR:    c.nodeCIDR,
		highlyAvail: c.highlyAvail,
	}, nil
}

// resolve fills in the credential (flag > env > .env file > interactive prompt)
// and the default kubeconfig path. needCreds is false for commands that only talk
// to the cluster, so `versions` works without an Aruba credential at hand.
func (c *config) resolve(needCreds bool) error {
	if c.kubeconfig == "" {
		c.kubeconfig = c.clusterName + ".kubeconfig"
	}
	if !needCreds {
		return nil
	}

	if c.clientID == "" {
		c.clientID = os.Getenv("ARUBA_CLIENT_ID")
	}
	if c.clientSecret == "" {
		c.clientSecret = os.Getenv("ARUBA_CLIENT_SECRET")
	}
	if c.clientID == "" || c.clientSecret == "" {
		env := readEnvFile(c.envFile)
		if c.clientID == "" {
			c.clientID = env["ARUBA_CLIENT_ID"]
		}
		if c.clientSecret == "" {
			c.clientSecret = env["ARUBA_CLIENT_SECRET"]
		}
	}
	var err error
	if c.clientID == "" {
		if c.clientID, err = prompt("Aruba client id: ", false); err != nil {
			return err
		}
	}
	if c.clientSecret == "" {
		if c.clientSecret, err = prompt("Aruba client secret: ", true); err != nil {
			return err
		}
	}
	if c.clientID == "" || c.clientSecret == "" {
		return errors.New("an Aruba client id and secret are required")
	}
	return nil
}

// readEnvFile parses the same gitignored .env the shell scripts source, so the
// existing setup keeps working. Only KEY=VALUE lines are understood.
func readEnvFile(path string) map[string]string {
	paths := []string{path}
	if path == "" {
		paths = []string{".env", "../.env"}
	}
	out := map[string]string{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
		return out
	}
	return out
}

func prompt(label string, secret bool) (string, error) {
	fmt.Fprint(os.Stderr, label)
	if secret && term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

// confirm guards the two destructive commands. --yes, or a non-interactive stdin,
// skips the question.
func (c *config) confirm(cmd *cobra.Command, format string, a ...any) bool {
	if yes, _ := cmd.Flags().GetBool("yes"); yes {
		return true
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Fprintf(os.Stderr, "About to "+format+".\nType yes to continue: ", a...)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line) == "yes"
}
