// Package namespaces provides the functionality to manage namespaces.
package namespaces

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"go.uber.org/zap"
	"helm.sh/helm/v3/pkg/cli/values"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/percona/everest/pkg/cli/helm"
	helmutils "github.com/percona/everest/pkg/cli/helm/utils"
	"github.com/percona/everest/pkg/cli/steps"
	cliutils "github.com/percona/everest/pkg/cli/utils"
	"github.com/percona/everest/pkg/common"
	"github.com/percona/everest/pkg/kubernetes"
	. "github.com/percona/everest/pkg/utils/must" //nolint:revive,stylecheck
	"github.com/percona/everest/pkg/version"
)

const (
	// Path to the everest-db-namespace subchart, relative to the main chart.
	dbNamespaceSubChartPath = "/charts/everest-db-namespace"
)

//nolint:gochecknoglobals
var (
	// ErrNSEmpty appears when the provided list of the namespaces is considered empty.
	ErrNSEmpty = errors.New("namespace list is empty. Specify at least one namespace")
	// ErrNSReserved appears when some of the provided names are forbidden to use.
	ErrNSReserved = func(ns string) error {
		return fmt.Errorf("'%s' namespace is reserved for Everest internals. Please specify another namespace", ns)
	}
	// ErrNameNotRFC1035Compatible appears when some of the provided names are not RFC1035 compatible.
	ErrNameNotRFC1035Compatible = func(fieldName string) error {
		return fmt.Errorf(`'%s' is not RFC 1035 compatible. The name should contain only lowercase alphanumeric characters or '-', start with an alphabetic character, end with an alphanumeric character`,
			fieldName,
		)
	}
	// ErrNoOperatorsSelected appears when no operators are selected for installation.
	ErrNoOperatorsSelected = errors.New("no operators selected for installation. Minimum one operator must be selected")
)

// NewNamespaceAdd returns a new CLI operation to add namespaces.
func NewNamespaceAdd(c NamespaceAddConfig, l *zap.SugaredLogger) (*NamespaceAdder, error) {
	n := &NamespaceAdder{
		cfg: c,
		l:   l.With("component", "namespace-adder"),
	}
	if c.Pretty {
		n.l = zap.NewNop().Sugar()
	}

	k, err := kubernetes.New(c.KubeconfigPath, n.l)
	if err != nil {
		var u *url.Error
		if errors.As(err, &u) {
			l.Error("Could not connect to Kubernetes. " +
				"Make sure Kubernetes is running and is accessible from this computer/server.")
		}
		return nil, err
	}
	n.kubeClient = k
	return n, nil
}

// NamespaceAddConfig is the configuration for adding namespaces.
type NamespaceAddConfig struct {
	// Namespaces to install.
	Namespaces string `mapstructure:"namespaces"`
	// SkipWizard is set if the wizard should be skipped.
	SkipWizard bool `mapstructure:"skip-wizard"`
	// KubeconfigPath is the path to the kubeconfig file.
	KubeconfigPath string `mapstructure:"kubeconfig"`
	// DisableTelemetry is set if telemetry should be disabled.
	DisableTelemetry bool `mapstructure:"disable-telemetry"`
	// TakeOwnership of an existing namespace.
	TakeOwnership bool `mapstructure:"take-ownership"`

	Operator OperatorConfig

	// Pretty print the output.
	Pretty bool

	// Update is set if the existing namespace needs to be updated.
	// This flag is set internally only, so that the add functionality may
	// be re-used for updating the namespace as well.
	Update bool
	// NamespaceList is a list of namespaces to install.
	// This is populated internally after validating the Namespaces field.:
	NamespaceList []string

	helm.CLIOptions
}

// OperatorConfig identifies which operators shall be installed.
type OperatorConfig struct {
	// PG stores if PostgresSQL shall be installed.
	PG bool `mapstructure:"postgresql"`
	// PSMDB stores if MongoDB shall be installed.
	PSMDB bool `mapstructure:"mongodb"`
	// PXC stores if XtraDB Cluster shall be installed.
	PXC bool `mapstructure:"xtradb-cluster"`
}

// NamespaceAdder provides the functionality to add namespaces.
type NamespaceAdder struct {
	l          *zap.SugaredLogger
	cfg        NamespaceAddConfig
	kubeClient *kubernetes.Kubernetes
}

// Run namespace add operation.
func (n *NamespaceAdder) Run(ctx context.Context) error {
	// This command expects a Helm based installation (< 1.4.0)
	ver, err := cliutils.CheckHelmInstallation(ctx, n.kubeClient)
	if err != nil {
		return err
	}

	installSteps := []steps.Step{}
	if version.IsDev(ver) && n.cfg.ChartDir == "" {
		cleanup, err := helmutils.SetupEverestDevChart(n.l, &n.cfg.ChartDir)
		if err != nil {
			return err
		}
		defer cleanup()
	}

	for _, namespace := range n.cfg.NamespaceList {
		installSteps = append(installSteps,
			n.newStepInstallNamespace(ver, namespace),
		)
	}

	var out io.Writer = os.Stdout
	if !n.cfg.Pretty {
		out = io.Discard
	}

	if err := steps.RunStepsWithSpinner(ctx, installSteps, out); err != nil {
		return err
	}
	return nil
}

func (n *NamespaceAdder) getValues() values.Options {
	v := []string{}
	v = append(v, "cleanupOnUninstall=false") // uninstall command will do the clean-up on its own.
	v = append(v, fmt.Sprintf("pxc=%t", n.cfg.Operator.PXC))
	v = append(v, fmt.Sprintf("postgresql=%t", n.cfg.Operator.PG))
	v = append(v, fmt.Sprintf("psmdb=%t", n.cfg.Operator.PSMDB))
	v = append(v, fmt.Sprintf("telemetry=%t", !n.cfg.DisableTelemetry))
	return values.Options{Values: v}
}

func (n *NamespaceAdder) newStepInstallNamespace(version, namespace string) steps.Step {
	action := "Installing"
	if n.cfg.Update {
		action = "Updating"
	}
	return steps.Step{
		Desc: fmt.Sprintf("%s namespace '%s'", action, namespace),
		F: func(ctx context.Context) error {
			return n.provisionDBNamespace(ctx, version, namespace)
		},
	}
}

func (n *NamespaceAdder) provisionDBNamespace(
	ctx context.Context,
	version string,
	namespace string,
) error {
	nsExists, ownedByEverest, err := n.namespaceExists(ctx, namespace)
	if err != nil {
		return err
	}

	if n.cfg.Update {
		if !nsExists {
			return fmt.Errorf("namespace (%s) does not exist", namespace)
		}
		if !ownedByEverest {
			return fmt.Errorf("namespace (%s) is not managed by Everest", namespace)
		}
	} else if nsExists && !n.cfg.TakeOwnership {
		return fmt.Errorf("namespace (%s) already exists", namespace)
	}

	chartDir := ""
	if n.cfg.ChartDir != "" {
		chartDir = path.Join(n.cfg.ChartDir, dbNamespaceSubChartPath)
	}
	values := Must(helmutils.MergeVals(n.getValues(), nil))
	installer := helm.Installer{
		ReleaseName:            namespace,
		ReleaseNamespace:       namespace,
		Values:                 values,
		CreateReleaseNamespace: !nsExists,
	}
	if err := installer.Init(n.cfg.KubeconfigPath, helm.ChartOptions{
		Directory: chartDir,
		URL:       n.cfg.RepoURL,
		Name:      helm.EverestDBNamespaceChartName,
		Version:   version,
	}); err != nil {
		return fmt.Errorf("could not initialize Helm installer: %w", err)
	}
	n.l.Infof("Installing DB namespace Helm chart in namespace ", namespace)
	return installer.Install(ctx)
}

func (n *NamespaceAdder) namespaceExists(ctx context.Context, namespace string) (bool, bool, error) {
	ns, err := n.kubeClient.GetNamespace(ctx, namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("cannot check if namesapce exists: %w", err)
	}
	return true, isManagedByEverest(ns), nil
}

func isManagedByEverest(ns *v1.Namespace) bool {
	val, ok := ns.GetLabels()[common.KubernetesManagedByLabel]
	return ok && val == common.Everest
}

// Populate the configuration with the required values.
func (cfg *NamespaceAddConfig) Populate(askNamespaces, askOperators bool) error {
	if err := cfg.populateNamespaces(askNamespaces); err != nil {
		return err
	}

	if askOperators && len(cfg.NamespaceList) > 0 && !cfg.SkipWizard {
		if err := cfg.populateOperators(); err != nil {
			return err
		}
	}

	return nil
}

func (cfg *NamespaceAddConfig) populateNamespaces(wizard bool) error {
	namespaces := cfg.Namespaces
	// no namespaces provided, ask the user
	if wizard && !cfg.SkipWizard {
		pNamespace := &survey.Input{
			Message: "Namespaces managed by Everest [comma separated]",
			Default: cfg.Namespaces,
		}
		if err := survey.AskOne(pNamespace, &namespaces); err != nil {
			return err
		}
	}

	list, err := ValidateNamespaces(namespaces)
	if err != nil {
		return err
	}
	cfg.NamespaceList = list
	return nil
}

func (cfg *NamespaceAddConfig) populateOperators() error {
	operatorOpts := []struct {
		label    string
		boolFlag *bool
	}{
		{"MySQL", &cfg.Operator.PXC},
		{"MongoDB", &cfg.Operator.PSMDB},
		{"PostgreSQL", &cfg.Operator.PG},
	}
	operatorLabels := make([]string, 0, len(operatorOpts))
	for _, v := range operatorOpts {
		operatorLabels = append(operatorLabels, v.label)
	}
	operatorDefaults := make([]string, 0, len(operatorOpts))
	for _, v := range operatorOpts {
		if *v.boolFlag {
			operatorDefaults = append(operatorDefaults, v.label)
		}
	}

	pOps := &survey.MultiSelect{
		Message: "Which operators do you want to install?",
		Default: operatorDefaults,
		Options: operatorLabels,
	}
	opIndexes := []int{}
	if err := survey.AskOne(
		pOps,
		&opIndexes,
	); err != nil {
		return err
	}

	if len(opIndexes) == 0 && len(cfg.NamespaceList) > 0 {
		return ErrNoOperatorsSelected
	}

	// We reset all flags to false so we select only
	// the ones which the user selected in the multiselect.
	for _, op := range operatorOpts {
		*op.boolFlag = false
	}

	for _, i := range opIndexes {
		*operatorOpts[i].boolFlag = true
	}

	return nil
}

// ValidateNamespaces validates a comma-separated namespaces string.
func ValidateNamespaces(str string) ([]string, error) {
	nsList := strings.Split(str, ",")
	m := make(map[string]struct{})
	for _, ns := range nsList {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}

		if ns == common.SystemNamespace || ns == common.MonitoringNamespace || ns == kubernetes.OLMNamespace {
			return nil, ErrNSReserved(ns)
		}

		if err := validateRFC1035(ns); err != nil {
			return nil, err
		}

		m[ns] = struct{}{}
	}

	list := make([]string, 0, len(m))
	for k := range m {
		list = append(list, k)
	}
	if len(list) == 0 {
		return nil, ErrNSEmpty
	}

	return list, nil
}

// validates names to be RFC-1035 compatible  https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#rfc-1035-label-names
func validateRFC1035(s string) error {
	rfc1035Regex := "^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$"
	re := regexp.MustCompile(rfc1035Regex)
	if !re.MatchString(s) {
		return ErrNameNotRFC1035Compatible(s)
	}

	return nil
}
