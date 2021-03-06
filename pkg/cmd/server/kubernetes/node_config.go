package kubernetes

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

	proxyoptions "k8s.io/kubernetes/cmd/kube-proxy/app/options"
	kubeletapp "k8s.io/kubernetes/cmd/kubelet/app"
	kubeletoptions "k8s.io/kubernetes/cmd/kubelet/app/options"
	kapi "k8s.io/kubernetes/pkg/api"
	kerrs "k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/componentconfig"
	"k8s.io/kubernetes/pkg/client/cache"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	clientadapter "k8s.io/kubernetes/pkg/client/unversioned/adapters/internalclientset"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"
	kubeletserver "k8s.io/kubernetes/pkg/kubelet/server"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	kcrypto "k8s.io/kubernetes/pkg/util/crypto"
	kerrors "k8s.io/kubernetes/pkg/util/errors"

	osclient "github.com/openshift/origin/pkg/client"
	configapi "github.com/openshift/origin/pkg/cmd/server/api"
	"github.com/openshift/origin/pkg/cmd/server/crypto"
	cmdutil "github.com/openshift/origin/pkg/cmd/util"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"
	cmdflags "github.com/openshift/origin/pkg/cmd/util/flags"
	"github.com/openshift/origin/pkg/cmd/util/variable"
	"github.com/openshift/origin/pkg/dns"
	sdnapi "github.com/openshift/origin/pkg/sdn/api"
	sdnplugin "github.com/openshift/origin/pkg/sdn/plugin"
	sdnpluginapi "github.com/openshift/origin/pkg/sdn/plugin/api"
)

// NodeConfig represents the required parameters to start the OpenShift node
// through Kubernetes. All fields are required.
type NodeConfig struct {
	// BindAddress is the address to bind to
	BindAddress string
	// VolumeDir is the directory that volumes will be stored under
	VolumeDir string
	// AllowDisabledDocker if true, will make the Kubelet ignore errors from Docker
	AllowDisabledDocker bool
	// Containerized is true if we are expected to be running inside of a container
	Containerized bool

	// Client to connect to the master.
	Client *client.Client
	// DockerClient is a client to connect to Docker
	DockerClient dockertools.DockerInterface
	// KubeletServer contains the KubeletServer configuration
	KubeletServer *kubeletoptions.KubeletServer
	// KubeletConfig is the configuration for the kubelet, fully initialized
	KubeletConfig *kubeletapp.KubeletConfig
	// ProxyConfig is the configuration for the kube-proxy, fully initialized
	ProxyConfig *proxyoptions.ProxyServerConfig
	// IPTablesSyncPeriod is how often iptable rules are refreshed
	IPTablesSyncPeriod string

	// ServiceStore is reused between proxy and DNS
	ServiceStore cache.Store
	// EndpointsStore is reused between proxy and DNS
	EndpointsStore cache.Store
	// ServicesReady is closed when the service and endpoint stores are ready to be used
	ServicesReady chan struct{}

	// DNSConfig controls the DNS configuration.
	DNSServer *dns.Server

	// SDNPlugin is an optional SDN plugin
	SDNPlugin sdnpluginapi.OsdnNodePlugin
	// EndpointsFilterer is an optional endpoints filterer
	FilteringEndpointsHandler sdnpluginapi.FilteringEndpointsConfigHandler
}

func BuildKubernetesNodeConfig(options configapi.NodeConfig, enableProxy, enableDNS bool) (*NodeConfig, error) {
	originClient, osClientConfig, err := configapi.GetOpenShiftClient(options.MasterKubeConfig)
	if err != nil {
		return nil, err
	}
	kubeClient, _, err := configapi.GetKubeClient(options.MasterKubeConfig)
	if err != nil {
		return nil, err
	}
	// Make a separate client for event reporting, to avoid event QPS blocking node calls
	eventClient, _, err := configapi.GetKubeClient(options.MasterKubeConfig)
	if err != nil {
		return nil, err
	}

	if options.NodeName == "localhost" {
		glog.Warningf(`Using "localhost" as node name will not resolve from all locations`)
	}

	clientCAs, err := kcrypto.CertPoolFromFile(options.ServingInfo.ClientCA)
	if err != nil {
		return nil, err
	}

	imageTemplate := variable.NewDefaultImageTemplate()
	imageTemplate.Format = options.ImageConfig.Format
	imageTemplate.Latest = options.ImageConfig.Latest

	var path string
	var fileCheckInterval int64
	if options.PodManifestConfig != nil {
		path = options.PodManifestConfig.Path
		fileCheckInterval = options.PodManifestConfig.FileCheckIntervalSeconds
	}

	var dockerExecHandler dockertools.ExecHandler

	switch options.DockerConfig.ExecHandlerName {
	case configapi.DockerExecHandlerNative:
		dockerExecHandler = &dockertools.NativeExecHandler{}
	case configapi.DockerExecHandlerNsenter:
		dockerExecHandler = &dockertools.NsenterExecHandler{}
	}

	kubeAddressStr, kubePortStr, err := net.SplitHostPort(options.ServingInfo.BindAddress)
	if err != nil {
		return nil, fmt.Errorf("cannot parse node address: %v", err)
	}
	kubePort, err := strconv.Atoi(kubePortStr)
	if err != nil {
		return nil, fmt.Errorf("cannot parse node port: %v", err)
	}

	options.NetworkConfig.NetworkPluginName, err = validateAndGetNetworkPluginName(originClient, options.NetworkConfig.NetworkPluginName)
	if err != nil {
		return nil, err
	}

	// Defaults are tested in TestKubeletDefaults
	server := kubeletoptions.NewKubeletServer()
	// Adjust defaults
	server.Config = path
	server.RootDirectory = options.VolumeDirectory
	server.NodeIP = options.NodeIP
	server.HostnameOverride = options.NodeName
	server.AllowPrivileged = true
	server.RegisterNode = true
	server.Address = kubeAddressStr
	server.Port = uint(kubePort)
	server.ReadOnlyPort = 0        // no read only access
	server.CAdvisorPort = 0        // no unsecured cadvisor access
	server.HealthzPort = 0         // no unsecured healthz access
	server.HealthzBindAddress = "" // no unsecured healthz access
	server.ClusterDNS = options.DNSIP
	server.ClusterDomain = options.DNSDomain
	server.NetworkPluginName = options.NetworkConfig.NetworkPluginName
	server.HostNetworkSources = strings.Join([]string{kubelettypes.ApiserverSource, kubelettypes.FileSource}, ",")
	server.HostPIDSources = strings.Join([]string{kubelettypes.ApiserverSource, kubelettypes.FileSource}, ",")
	server.HostIPCSources = strings.Join([]string{kubelettypes.ApiserverSource, kubelettypes.FileSource}, ",")
	server.HTTPCheckFrequency = unversioned.Duration{Duration: time.Duration(0)} // no remote HTTP pod creation access
	server.FileCheckFrequency = unversioned.Duration{Duration: time.Duration(fileCheckInterval) * time.Second}
	server.PodInfraContainerImage = imageTemplate.ExpandOrDie("pod")
	server.CPUCFSQuota = true // enable cpu cfs quota enforcement by default
	server.MaxPods = 110
	server.SerializeImagePulls = false // disable serial image pulls by default
	if enableDNS {
		// if we are running local DNS, skydns will load the default recursive nameservers for us
		server.ResolverConfig = ""
	}

	if sdnplugin.IsOpenShiftNetworkPlugin(server.NetworkPluginName) {
		// set defaults for openshift-sdn
		server.HairpinMode = componentconfig.HairpinNone
		server.ConfigureCBR0 = false
	}

	// prevents kube from generating certs
	server.TLSCertFile = options.ServingInfo.ServerCert.CertFile
	server.TLSPrivateKeyFile = options.ServingInfo.ServerCert.KeyFile

	containerized := cmdutil.Env("OPENSHIFT_CONTAINERIZED", "") == "true"
	server.Containerized = containerized

	// resolve extended arguments
	// TODO: this should be done in config validation (along with the above) so we can provide
	// proper errors
	if err := cmdflags.Resolve(options.KubeletArguments, server.AddFlags); len(err) > 0 {
		return nil, kerrors.NewAggregate(err)
	}

	proxyconfig, err := buildKubeProxyConfig(options)
	if err != nil {
		return nil, err
	}

	cfg, err := kubeletapp.UnsecuredKubeletConfig(server)
	if err != nil {
		return nil, err
	}

	// provide any config overrides
	cfg.NodeName = options.NodeName
	cfg.KubeClient = clientadapter.FromUnversionedClient(kubeClient)
	cfg.EventClient = clientadapter.FromUnversionedClient(eventClient)
	cfg.DockerExecHandler = dockerExecHandler

	// Setup auth
	authnTTL, err := time.ParseDuration(options.AuthConfig.AuthenticationCacheTTL)
	if err != nil {
		return nil, err
	}
	authn, err := newAuthenticator(clientCAs, clientcmd.AnonymousClientConfig(osClientConfig), authnTTL, options.AuthConfig.AuthenticationCacheSize)
	if err != nil {
		return nil, err
	}

	authzAttr, err := newAuthorizerAttributesGetter(options.NodeName)
	if err != nil {
		return nil, err
	}

	authzTTL, err := time.ParseDuration(options.AuthConfig.AuthorizationCacheTTL)
	if err != nil {
		return nil, err
	}
	authz, err := newAuthorizer(originClient, authzTTL, options.AuthConfig.AuthorizationCacheSize)
	if err != nil {
		return nil, err
	}

	cfg.Auth = kubeletserver.NewKubeletAuth(authn, authzAttr, authz)

	// Make sure the node doesn't think it is in standalone mode
	// This is required for the node to enforce nodeSelectors on pods, to set hostIP on pod status updates, etc
	cfg.StandaloneMode = false

	// TODO: could be cleaner
	if configapi.UseTLS(options.ServingInfo) {
		extraCerts, err := configapi.GetNamedCertificateMap(options.ServingInfo.NamedCertificates)
		if err != nil {
			return nil, err
		}
		cfg.TLSOptions = &kubeletserver.TLSOptions{
			Config: crypto.SecureTLSConfig(&tls.Config{
				// RequestClientCert lets us request certs, but allow requests without client certs
				// Verification is done by the authn layer
				ClientAuth: tls.RequestClientCert,
				ClientCAs:  clientCAs,
				// Set SNI certificate func
				// Do not use NameToCertificate, since that requires certificates be included in the server's tlsConfig.Certificates list,
				// which we do not control when running with http.Server#ListenAndServeTLS
				GetCertificate: cmdutil.GetCertificateFunc(extraCerts),
			}),
			CertFile: options.ServingInfo.ServerCert.CertFile,
			KeyFile:  options.ServingInfo.ServerCert.KeyFile,
		}
	} else {
		cfg.TLSOptions = nil
	}

	// Prepare cloud provider
	cloud, err := cloudprovider.InitCloudProvider(server.CloudProvider, server.CloudConfigFile)
	if err != nil {
		return nil, err
	}
	if cloud != nil {
		glog.V(2).Infof("Successfully initialized cloud provider: %q from the config file: %q\n", server.CloudProvider, server.CloudConfigFile)
	}
	cfg.Cloud = cloud

	iptablesSyncPeriod, err := time.ParseDuration(options.IPTablesSyncPeriod)
	if err != nil {
		return nil, fmt.Errorf("Cannot parse the provided ip-tables sync period (%s) : %v", options.IPTablesSyncPeriod, err)
	}
	sdnPlugin, err := sdnplugin.NewNodePlugin(options.NetworkConfig.NetworkPluginName, originClient, kubeClient, options.NodeName, options.NodeIP, iptablesSyncPeriod, options.NetworkConfig.MTU)
	if err != nil {
		return nil, fmt.Errorf("SDN initialization failed: %v", err)
	}
	if sdnPlugin != nil {
		cfg.NetworkPlugins = append(cfg.NetworkPlugins, sdnPlugin)
	}

	endpointFilter, err := sdnplugin.NewProxyPlugin(options.NetworkConfig.NetworkPluginName, originClient, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("SDN proxy initialization failed: %v", err)
	}

	config := &NodeConfig{
		BindAddress: options.ServingInfo.BindAddress,

		AllowDisabledDocker: options.AllowDisabledDocker,
		Containerized:       containerized,

		Client: kubeClient,

		VolumeDir: options.VolumeDirectory,

		KubeletServer: server,
		KubeletConfig: cfg,

		ServicesReady: make(chan struct{}),

		ProxyConfig: proxyconfig,

		SDNPlugin:                 sdnPlugin,
		FilteringEndpointsHandler: endpointFilter,
	}

	if enableDNS {
		dnsConfig, err := dns.NewServerDefaults()
		if err != nil {
			return nil, fmt.Errorf("DNS configuration was not possible: %v", err)
		}
		if len(options.DNSIP) > 0 {
			dnsConfig.DnsAddr = options.DNSIP + ":53"
		}
		dnsConfig.Domain = server.ClusterDomain + "."
		dnsConfig.Local = "openshift.default.svc." + dnsConfig.Domain

		services, serviceStore := dns.NewCachedServiceAccessorAndStore()
		endpoints, endpointsStore := dns.NewCachedEndpointsAccessorAndStore()
		if !enableProxy {
			endpoints = kubeClient
			endpointsStore = nil
		}

		// TODO: use kubeletConfig.ResolverConfig as an argument to etcd in the event the
		//   user sets it, instead of passing it to the kubelet.

		config.ServiceStore = serviceStore
		config.EndpointsStore = endpointsStore
		config.DNSServer = &dns.Server{
			Config:      dnsConfig,
			Services:    services,
			Endpoints:   endpoints,
			MetricsName: "node",
		}
	}

	return config, nil
}

func buildKubeProxyConfig(options configapi.NodeConfig) (*proxyoptions.ProxyServerConfig, error) {
	// get default config
	proxyconfig := proxyoptions.NewProxyConfig()

	// BindAddress - Override default bind address from our config
	addr := options.ServingInfo.BindAddress
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("The provided value to bind to must be an ip:port %q", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("The provided value to bind to must be an ip:port: %q", addr)
	}
	proxyconfig.BindAddress = ip.String()

	// HealthzPort, HealthzBindAddress - disable
	proxyconfig.HealthzPort = 0
	proxyconfig.HealthzBindAddress = ""

	// OOMScoreAdj, ResourceContainer - clear, we don't run in a container
	oomScoreAdj := int32(0)
	proxyconfig.OOMScoreAdj = &oomScoreAdj
	proxyconfig.ResourceContainer = ""

	// use the same client as the node
	proxyconfig.Master = ""
	proxyconfig.Kubeconfig = options.MasterKubeConfig

	// PortRange, use default
	// HostnameOverride, use default

	// ProxyMode, set to iptables
	proxyconfig.Mode = "iptables"

	// IptablesSyncPeriod, set to our config value
	syncPeriod, err := time.ParseDuration(options.IPTablesSyncPeriod)
	if err != nil {
		return nil, fmt.Errorf("Cannot parse the provided ip-tables sync period (%s) : %v", options.IPTablesSyncPeriod, err)
	}
	proxyconfig.IPTablesSyncPeriod = unversioned.Duration{
		Duration: syncPeriod,
	}

	// ConfigSyncPeriod, use default

	// NodeRef, build from config
	proxyconfig.NodeRef = &kapi.ObjectReference{
		Kind: "Node",
		Name: options.NodeName,
	}

	// MasqueradeAll, use default

	// CleanupAndExit, use default

	// KubeAPIQPS, use default, doesn't apply until we build a separate client
	// KubeAPIBurst, use default, doesn't apply until we build a separate client

	// UDPIdleTimeout, use default

	// Resolve cmd flags to add any user overrides
	if err := cmdflags.Resolve(options.ProxyArguments, proxyconfig.AddFlags); len(err) > 0 {
		return nil, kerrors.NewAggregate(err)
	}

	return proxyconfig, nil
}

func validateAndGetNetworkPluginName(originClient *osclient.Client, pluginName string) (string, error) {
	if sdnplugin.IsOpenShiftNetworkPlugin(pluginName) {
		// Detect any plugin mismatches between node and master
		clusterNetwork, err := originClient.ClusterNetwork().Get(sdnapi.ClusterNetworkDefault)
		if kerrs.IsNotFound(err) {
			return "", fmt.Errorf("master has not created a default cluster network, network plugin %q can not start", pluginName)
		} else if err != nil {
			return "", fmt.Errorf("cannot fetch %q cluster network: %v", sdnapi.ClusterNetworkDefault, err)
		}

		if clusterNetwork.PluginName != strings.ToLower(pluginName) {
			if len(clusterNetwork.PluginName) != 0 {
				return "", fmt.Errorf("detected network plugin mismatch between OpenShift node(%q) and master(%q)", pluginName, clusterNetwork.PluginName)
			} else {
				// Do not return error in this case
				glog.Warningf(`either there is network plugin mismatch between OpenShift node(%q) and master or OpenShift master is running an older version where we did not persist plugin name`, pluginName)
			}
		}
	} else if pluginName == "" {
		// Auto detect network plugin configured by master
		clusterNetwork, err := originClient.ClusterNetwork().Get(sdnapi.ClusterNetworkDefault)
		if err == nil {
			return clusterNetwork.PluginName, nil
		}
	}

	return pluginName, nil
}
