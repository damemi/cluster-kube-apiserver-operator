package render

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"

	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	kubecontrolplanev1 "github.com/openshift/api/kubecontrolplane/v1"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/v311_00_assets"
	installertypes "github.com/openshift/installer/pkg/types"
	genericrender "github.com/openshift/library-go/pkg/operator/render"
	genericrenderoptions "github.com/openshift/library-go/pkg/operator/render/options"
	"github.com/openshift/library-go/pkg/operator/v1alpha1helpers"
)

const (
	bootstrapVersion = "v3.11.0"
)

// renderOpts holds values to drive the render command.
type renderOpts struct {
	manifest genericrenderoptions.ManifestOptions
	generic  genericrenderoptions.GenericOptions

	lockHostPath      string
	etcdServerURLs    []string
	etcdServingCA     string
	disablePhase2     bool
	clusterConfigFile string
}

// NewRenderCommand creates a render command.
func NewRenderCommand() *cobra.Command {
	renderOpts := renderOpts{
		generic:  *genericrenderoptions.NewGenericOptions(),
		manifest: *genericrenderoptions.NewManifestOptions("kube-apiserver", "openshift/origin-hypershift:latest"),

		lockHostPath:      "/var/run/kubernetes/lock",
		etcdServerURLs:    []string{"https://127.0.0.1:2379"},
		etcdServingCA:     "root-ca.crt",
		clusterConfigFile: "/opt/tectonic/manifests/cluster-config.yaml",
	}
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render kubernetes API server bootstrap manifests, secrets and configMaps",
		Run: func(cmd *cobra.Command, args []string) {
			if err := renderOpts.Validate(); err != nil {
				glog.Fatal(err)
			}
			if err := renderOpts.Complete(); err != nil {
				glog.Fatal(err)
			}
			if err := renderOpts.Run(); err != nil {
				glog.Fatal(err)
			}
		},
	}

	renderOpts.AddFlags(cmd.Flags())

	return cmd
}

func (r *renderOpts) AddFlags(fs *pflag.FlagSet) {
	r.manifest.AddFlags(fs, "apiserver")
	r.generic.AddFlags(fs, kubecontrolplanev1.GroupVersion.WithKind("KubeAPIServerConfig"))

	fs.StringVar(&r.lockHostPath, "manifest-lock-host-path", r.lockHostPath, "A host path mounted into the apiserver pods to hold lock.")
	fs.StringArrayVar(&r.etcdServerURLs, "manifest-etcd-server-urls", r.etcdServerURLs, "The etcd server URL, comma separated.")
	fs.StringVar(&r.etcdServingCA, "manifest-etcd-serving-ca", r.etcdServingCA, "The etcd serving CA.")
	fs.StringVar(&r.clusterConfigFile, "cluster-config-file", r.configFile, "ClusterConfig ConfigMap file.")

	// TODO: remove when the installer has stopped using it
	fs.BoolVar(&r.disablePhase2, "disable-phase-2", r.disablePhase2, "Disable rendering of the phase 2 daemonset and dependencies.")
	fs.MarkHidden("disable-phase-2")
	fs.MarkDeprecated("disable-phase-2", "Only used temporarily to synchronize roll out of the phase 2 removal. Does nothing anymore.")
}

// Validate verifies the inputs.
func (r *renderOpts) Validate() error {
	if err := r.manifest.Validate(); err != nil {
		return err
	}
	if err := r.generic.Validate(); err != nil {
		return err
	}

	if len(r.lockHostPath) == 0 {
		return errors.New("missing required flag: --manifest-lock-host-path")
	}
	if len(r.etcdServerURLs) == 0 {
		return errors.New("missing etcd server URLs: --manifest-etcd-server-urls")
	}
	if len(r.etcdServingCA) == 0 {
		return errors.New("missing etcd serving CA: --manifest-etcd-serving-ca")
	}
	if len(r.clusterConfigFile) == 0 {
		return errors.New("missing cluster ConfigMap file: --cluster-config-file")
	}

	return nil
}

// Complete fills in missing values before command execution.
func (r *renderOpts) Complete() error {
	if err := r.manifest.Complete(); err != nil {
		return err
	}
	if err := r.generic.Complete(); err != nil {
		return err
	}
	return nil
}

type TemplateData struct {
	genericrenderoptions.ManifestConfig
	genericrenderoptions.FileConfig

	// LockHostPath holds the api server lock file for bootstrap
	LockHostPath string

	// EtcdServerURLs is a list of etcd server URLs.
	EtcdServerURLs []string

	// EtcdServingCA is the serving CA used by the etcd servers.
	EtcdServingCA string

	// RestrictedCIDRs is a list of restricted CIDRs in the admissionPluginConfig
	RestrictedCIDRs []string
}

// Run contains the logic of the render command.
func (r *renderOpts) Run() error {
	clusterConfigFileData, err := ioutil.ReadFile(r.clusterConfigFile)
	if err != nil {
		return err
	}
	installConfig, err := v1alpha1helpers.InstallConfigFromFile(clusterConfigFileData)
	if err != nil {
		return err
	}
	restrictedCIDRs := discoverRestrictedCIDRs(installConfig)

	renderConfig := TemplateData{
		LockHostPath:    r.lockHostPath,
		EtcdServerURLs:  r.etcdServerURLs,
		EtcdServingCA:   r.etcdServingCA,
		RestrictedCIDRs: restrictedCIDRs,
	}
	if err := r.manifest.ApplyTo(&renderConfig.ManifestConfig); err != nil {
		return err
	}
	if err := r.generic.ApplyTo(
		&renderConfig.FileConfig,
		genericrenderoptions.Template{FileName: "defaultconfig.yaml", Content: v311_00_assets.MustAsset(filepath.Join(bootstrapVersion, "kube-apiserver", "defaultconfig.yaml"))},
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "bootstrap-config-overrides.yaml")),
		mustReadTemplateFile(filepath.Join(r.generic.TemplatesDir, "config", "config-overrides.yaml")),
		&renderConfig,
	); err != nil {
		return err
	}

	return genericrender.WriteFiles(&r.generic, &renderConfig.FileConfig, renderConfig)
}

func mustReadTemplateFile(fname string) genericrenderoptions.Template {
	bs, err := ioutil.ReadFile(fname)
	if err != nil {
		panic(fmt.Sprintf("Failed to load %q: %v", fname, err))
	}
	return genericrenderoptions.Template{FileName: fname, Content: bs}
}

func discoverRestrictedCIDRs(ic installertypes.InstallConfig) []string {
	restrictedCIDRs := []string{}
	restrictedCIDRs = append(restrictedCIDRs, ic.Networking.ServiceCIDR.String(), ic.Networking.PodCIDR.String())
	return restrictedCIDRs, nil
}
