package operands

import (
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/util/yaml"
	"os"
	"reflect"
	"strconv"
	"strings"

	hcov1beta1 "github.com/kubevirt/hyperconverged-cluster-operator/pkg/apis/hco/v1beta1"
	"github.com/kubevirt/hyperconverged-cluster-operator/pkg/controller/common"
	"github.com/kubevirt/hyperconverged-cluster-operator/pkg/util"
	hcoutil "github.com/kubevirt/hyperconverged-cluster-operator/pkg/util"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/client-go/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubevirtDefaultNetworkInterfaceValue = "masquerade"
	// We can import the constants below from Kubevirt virt-config package
	// after Kubevirt will consume k8s.io v0.19.2 or higher
	FeatureGatesKey         = "feature-gates"
	MachineTypeKey          = "machine-type"
	UseEmulationKey         = "debug.useEmulation"
	MigrationsConfigKey     = "migrations"
	NetworkInterfaceKey     = "default-network-interface"
	SmbiosConfigKey         = "smbios"
	SELinuxLauncherTypeKey  = "selinuxLauncherType"
	DefaultNetworkInterface = "bridge"
)

// env vars
const (
	kvmEmulationEnvName = "KVM_EMULATION"
	smbiosEnvName       = "SMBIOS"
	machineTypeEnvName  = "MACHINETYPE"
)

// KubeVirt hard coded FeatureGates
// These feature gates are set by HCO in the KubeVirt CR and can't be modified by the end user.
const (
	// indicates that we support turning on DataVolume workflows. This means using DataVolumes in the VM and VMI
	// definitions. There was a period of time where this was in alpha and needed to be explicility enabled.
	// It also means that someone is using KubeVirt with CDI. So by not enabling this feature gate, someone can safely
	// use kubevirt without CDI and know that users of kubevirt will not be able to post VM/VMIs that use CDI workflows
	// that aren't available to them
	kvDataVolumesGate = "DataVolumes"

	// Enable Single-root input/output virtualization
	kvSRIOVGate = "SRIOV"

	// Enables VMIs to be live migrated. Without this, migrations are not possible and will be blocked
	kvLiveMigrationGate = "LiveMigration"

	// Enables the CPUManager feature gate to label the nodes which have the Kubernetes CPUManager running. VMIs that
	// require dedicated CPU resources will automatically be scheduled on the labeled nodes
	kvCPUManagerGate = "CPUManager"

	// Enables schedule VMIs according to their CPU model
	kvCPUNodeDiscoveryGate = "CPUNodeDiscovery"

	// Enables using our sidecar hooks for injecting custom logic into the VMI startup flow. This is a very advanced
	// feature that has security implications, which is why it is opt-in only
	// TODO: Remove this feature gate because it creates a security issue:
	// it allows anyone to execute arbitrary third party code in the VMI pod.
	// Since the VMI pods execute with capabilities that a user may not actually
	// have, it's a path to privilege escalation.
	kvSidecarGate = "Sidecar"

	// Enables the alpha offline snapshot functionality
	kvSnapshotGate = "Snapshot"
)

var (
	hardCodeKvFgs = []string{
		kvDataVolumesGate,
		kvSRIOVGate,
		kvLiveMigrationGate,
		kvCPUManagerGate,
		kvCPUNodeDiscoveryGate,
		kvSidecarGate,
		kvSnapshotGate,
	}
)

// KubeVirt feature gates that are exposed in HCO API
const (
	HotplugVolumesGate       = "HotplugVolumes"
	kvWithHostPassthroughCPU = "WithHostPassthroughCPU"
	kvWithHostModelCPU       = "WithHostModelCPU"
	SRIOVLiveMigrationGate   = "SRIOVLiveMigration"
	kvHypervStrictCheck      = "HypervStrictCheck"
	GPUGate                  = "GPU"
	HostDevicesGate          = "HostDevices"
	SELinuxLauncherType      = "virt_launcher.process"
)

// ************  KubeVirt Handler  **************
type kubevirtHandler genericOperand

func newKubevirtHandler(Client client.Client, Scheme *runtime.Scheme) *kubevirtHandler {
	return &kubevirtHandler{
		Client:                 Client,
		Scheme:                 Scheme,
		crType:                 "KubeVirt",
		removeExistingOwner:    false,
		setControllerReference: true,
		isCr:                   true,
		hooks:                  &kubevirtHooks{},
	}
}

type kubevirtHooks struct {
	cache *kubevirtv1.KubeVirt
}

func (h *kubevirtHooks) getFullCr(hc *hcov1beta1.HyperConverged) (client.Object, error) {
	if h.cache == nil {
		kv, err := NewKubeVirt(hc)
		if err != nil {
			return nil, err
		}
		h.cache = kv
	}
	return h.cache, nil
}

func (h kubevirtHooks) getEmptyCr() client.Object                          { return &kubevirtv1.KubeVirt{} }
func (h kubevirtHooks) validate() error                                    { return nil }
func (h kubevirtHooks) postFound(*common.HcoRequest, runtime.Object) error { return nil }
func (h kubevirtHooks) getConditions(cr runtime.Object) []conditionsv1.Condition {
	return translateKubeVirtConds(cr.(*kubevirtv1.KubeVirt).Status.Conditions)
}
func (h kubevirtHooks) checkComponentVersion(cr runtime.Object) bool {
	found := cr.(*kubevirtv1.KubeVirt)
	return checkComponentVersion(hcoutil.KubevirtVersionEnvV, found.Status.ObservedKubeVirtVersion)
}
func (h kubevirtHooks) getObjectMeta(cr runtime.Object) *metav1.ObjectMeta {
	return &cr.(*kubevirtv1.KubeVirt).ObjectMeta
}
func (h *kubevirtHooks) reset() {
	h.cache = nil
}

func (h *kubevirtHooks) updateCr(req *common.HcoRequest, Client client.Client, exists runtime.Object, required runtime.Object) (bool, bool, error) {
	virt, ok1 := required.(*kubevirtv1.KubeVirt)
	found, ok2 := exists.(*kubevirtv1.KubeVirt)
	if !ok1 || !ok2 {
		return false, false, errors.New("can't convert to KubeVirt")
	}
	if !reflect.DeepEqual(found.Spec, virt.Spec) ||
		!reflect.DeepEqual(found.Labels, virt.Labels) {
		if req.HCOTriggered {
			req.Logger.Info("Updating existing KubeVirt's Spec to new opinionated values")
		} else {
			req.Logger.Info("Reconciling an externally updated KubeVirt's Spec to its opinionated values")
		}
		util.DeepCopyLabels(&virt.ObjectMeta, &found.ObjectMeta)
		virt.Spec.DeepCopyInto(&found.Spec)
		err := Client.Update(req.Ctx, found)
		if err != nil {
			return false, false, err
		}
		return true, !req.HCOTriggered, nil
	}
	return false, false, nil
}

func NewKubeVirt(hc *hcov1beta1.HyperConverged, opts ...string) (*kubevirtv1.KubeVirt, error) {
	config, err := getKVConfig(hc)
	if err != nil {
		return nil, err
	}

	spec := kubevirtv1.KubeVirtSpec{
		UninstallStrategy: kubevirtv1.KubeVirtUninstallStrategyBlockUninstallIfWorkloadsExist,
		Infra:             hcoConfig2KvConfig(hc.Spec.Infra),
		Workloads:         hcoConfig2KvConfig(hc.Spec.Workloads),
		Configuration:     *config,
	}

	kv := NewKubeVirtWithNameOnly(hc, opts...)
	kv.Spec = spec

	if err := applyPatchToSpec(hc, common.JSONPatchKVAnnotationName, kv); err != nil {
		return nil, err
	}

	return kv, nil
}

func getKVConfig(hc *hcov1beta1.HyperConverged) (*kubevirtv1.KubeVirtConfiguration, error) {
	devConfig, err := getKVDevConfig(hc)
	if err != nil {
		return nil, err
	}

	config := &kubevirtv1.KubeVirtConfiguration{
		DeveloperConfiguration: devConfig,
		SELinuxLauncherType:    SELinuxLauncherType,
		NetworkConfiguration: &kubevirtv1.NetworkConfiguration{
			NetworkInterface: string(kubevirtv1.MasqueradeInterface),
		},
	}

	if smbiosConfig, ok := os.LookupEnv(smbiosEnvName); ok {
		if smbiosConfig = strings.TrimSpace(smbiosConfig); smbiosConfig != "" {
			config.SMBIOSConfig = &kubevirtv1.SMBiosConfiguration{}
			err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(smbiosConfig), 1024).Decode(config.SMBIOSConfig)
			if err != nil {
				return config, err
			}
		}
	}

	if val, ok := os.LookupEnv(machineTypeEnvName); ok {
		if val = strings.TrimSpace(val); val != "" {
			config.MachineType = val
		}
	}

	return config, nil
}

func getKVDevConfig(hc *hcov1beta1.HyperConverged) (*kubevirtv1.DeveloperConfiguration, error) {
	fgs := getKvFeatureGateList(hc.Spec.FeatureGates)

	var kvmEmulation = false
	kvmEmulationStr, ok := os.LookupEnv(kvmEmulationEnvName)
	if ok {
		if kvmEmulationStr = strings.TrimSpace(kvmEmulationStr); kvmEmulationStr != "" {
			var err error
			kvmEmulation, err = strconv.ParseBool(kvmEmulationStr)
			if err != nil {
				return nil, err
			}
		}
	}

	if len(fgs) > 0 || kvmEmulation {
		return &kubevirtv1.DeveloperConfiguration{
			FeatureGates: fgs,
			UseEmulation: kvmEmulation,
		}, nil
	}

	return nil, nil
}

func NewKubeVirtWithNameOnly(hc *hcov1beta1.HyperConverged, opts ...string) *kubevirtv1.KubeVirt {
	return &kubevirtv1.KubeVirt{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubevirt-" + hc.Name,
			Labels:    getLabels(hc, hcoutil.AppComponentCompute),
			Namespace: getNamespace(hc.Namespace, opts),
		},
	}
}

func hcoConfig2KvConfig(hcoConfig hcov1beta1.HyperConvergedConfig) *kubevirtv1.ComponentConfig {
	if hcoConfig.NodePlacement != nil {
		kvConfig := &kubevirtv1.ComponentConfig{}
		kvConfig.NodePlacement = &kubevirtv1.NodePlacement{}

		if hcoConfig.NodePlacement.Affinity != nil {
			kvConfig.NodePlacement.Affinity = &corev1.Affinity{}
			hcoConfig.NodePlacement.Affinity.DeepCopyInto(kvConfig.NodePlacement.Affinity)
		}

		if hcoConfig.NodePlacement.NodeSelector != nil {
			kvConfig.NodePlacement.NodeSelector = make(map[string]string)
			for k, v := range hcoConfig.NodePlacement.NodeSelector {
				kvConfig.NodePlacement.NodeSelector[k] = v
			}
		}

		for _, hcoTolr := range hcoConfig.NodePlacement.Tolerations {
			kvTolr := corev1.Toleration{}
			hcoTolr.DeepCopyInto(&kvTolr)
			kvConfig.NodePlacement.Tolerations = append(kvConfig.NodePlacement.Tolerations, kvTolr)
		}

		return kvConfig
	}
	return nil
}

// ***********  KubeVirt Config Handler  ************
type kvConfigHandler genericOperand

func newKvConfigHandler(Client client.Client, Scheme *runtime.Scheme) *kvConfigHandler {
	return &kvConfigHandler{
		Client:                 Client,
		Scheme:                 Scheme,
		crType:                 "KubeVirtConfig",
		removeExistingOwner:    false,
		setControllerReference: false,
		isCr:                   false,
		hooks:                  &kvConfigHooks{},
	}
}

type kvConfigHooks struct{}

func (h kvConfigHooks) getFullCr(hc *hcov1beta1.HyperConverged) (client.Object, error) {
	return NewKubeVirtConfigForCR(hc, hc.Namespace), nil
}
func (h kvConfigHooks) getEmptyCr() client.Object                             { return &corev1.ConfigMap{} }
func (h kvConfigHooks) validate() error                                       { return nil }
func (h kvConfigHooks) postFound(*common.HcoRequest, runtime.Object) error    { return nil }
func (h kvConfigHooks) getConditions(runtime.Object) []conditionsv1.Condition { return nil }
func (h kvConfigHooks) checkComponentVersion(runtime.Object) bool             { return true }
func (h kvConfigHooks) getObjectMeta(cr runtime.Object) *metav1.ObjectMeta {
	return &cr.(*corev1.ConfigMap).ObjectMeta
}
func (h kvConfigHooks) reset() { /* no implementation */ }

func (h *kvConfigHooks) updateCr(req *common.HcoRequest, Client client.Client, exists runtime.Object, required runtime.Object) (bool, bool, error) {
	kubevirtConfig, ok1 := required.(*corev1.ConfigMap)
	found, ok2 := exists.(*corev1.ConfigMap)
	if !ok1 || !ok2 {
		return false, false, errors.New("can't convert to ConfigMap")
	}

	changed := false
	if req.UpgradeMode {
		changed = h.updateDataOnUpgrade(req, found, kubevirtConfig)
	}

	changed = h.updateData(found, kubevirtConfig) || changed

	if !reflect.DeepEqual(found.Labels, kubevirtConfig.Labels) {
		util.DeepCopyLabels(&kubevirtConfig.ObjectMeta, &found.ObjectMeta)
		changed = true
	}

	if changed {
		return h.updateKvConfigMap(req, Client, found)
	}

	return false, false, nil
}
func (h *kvConfigHooks) updateDataOnUpgrade(req *common.HcoRequest, found *corev1.ConfigMap, kubevirtConfig *corev1.ConfigMap) bool {
	changed := false
	if h.forceDefaultKeys(req, found, kubevirtConfig) {
		changed = true
	}

	if h.removeOldKeys(req, found) {
		changed = true
	}

	return changed
}

func (h *kvConfigHooks) updateData(found *corev1.ConfigMap, required *corev1.ConfigMap) bool {
	if found.Data[FeatureGatesKey] != required.Data[FeatureGatesKey] {
		found.Data[FeatureGatesKey] = required.Data[FeatureGatesKey]
		return true
	}

	return false
}

func (h *kvConfigHooks) updateKvConfigMap(req *common.HcoRequest, Client client.Client, found *corev1.ConfigMap) (bool, bool, error) {
	err := Client.Update(req.Ctx, found)
	if err != nil {
		req.Logger.Error(err, "Failed updating the kubevirt config map")
		return false, false, err
	}
	return true, false, nil
}

type featureGateChecks map[string]func() bool

func getFeatureGateChecks(featureGates *hcov1beta1.HyperConvergedFeatureGates) featureGateChecks {
	return map[string]func() bool{
		HotplugVolumesGate:       featureGates.IsHotplugVolumesEnabled,
		kvWithHostPassthroughCPU: featureGates.IsWithHostPassthroughCPUEnabled,
		kvWithHostModelCPU:       featureGates.IsWithHostModelCPUEnabled,
		SRIOVLiveMigrationGate:   featureGates.IsSRIOVLiveMigrationEnabled,
		kvHypervStrictCheck:      featureGates.IsHypervStrictCheckEnabled,
		GPUGate:                  featureGates.IsGPUAssignmentEnabled,
		HostDevicesGate:          featureGates.IsHostDevicesAssignmentEnabled,
	}
}

func (h *kvConfigHooks) forceDefaultKeys(req *common.HcoRequest, found *corev1.ConfigMap, kubevirtConfig *corev1.ConfigMap) bool {
	changed := false
	// only virtconfig.SmbiosConfigKey, virtconfig.MachineTypeKey, virtconfig.SELinuxLauncherTypeKey,
	// virtconfig.FeatureGatesKey and virtconfig.UseEmulationKey are going to be manipulated
	// and only on HCO upgrades.
	// virtconfig.MigrationsConfigKey is going to be removed if set in the past (only during upgrades).
	// TODO: This is going to change in the next HCO release where the whole configMap is going
	// to be continuously reconciled
	for _, k := range []string{
		SmbiosConfigKey,
		MachineTypeKey,
		SELinuxLauncherTypeKey,
		UseEmulationKey,
	} {
		// don't change the order. putting "changed" as the first part of the condition will cause skipping the
		// implementation of the forceDefaultValues function.
		changed = h.forceDefaultValues(req, found, kubevirtConfig, k) || changed
	}

	return changed
}

func (h *kvConfigHooks) removeOldKeys(req *common.HcoRequest, found *corev1.ConfigMap) bool {
	if _, ok := found.Data[MigrationsConfigKey]; ok {
		req.Logger.Info(fmt.Sprintf("Deleting %s on existing KubeVirt config", MigrationsConfigKey))
		delete(found.Data, MigrationsConfigKey)
		return true
	}
	return false

}

func (h *kvConfigHooks) forceDefaultValues(req *common.HcoRequest, found *corev1.ConfigMap, kubevirtConfig *corev1.ConfigMap, k string) bool {
	if found.Data[k] != kubevirtConfig.Data[k] {
		req.Logger.Info(fmt.Sprintf("Updating %s on existing KubeVirt config", k))
		found.Data[k] = kubevirtConfig.Data[k]
		return true
	}
	return false
}

// ***********  KubeVirt Priority Class  ************
type kvPriorityClassHandler genericOperand

func newKvPriorityClassHandler(Client client.Client, Scheme *runtime.Scheme) *kvPriorityClassHandler {
	return &kvPriorityClassHandler{
		Client:                 Client,
		Scheme:                 Scheme,
		crType:                 "KubeVirtPriorityClass",
		removeExistingOwner:    false,
		setControllerReference: false,
		isCr:                   false,
		hooks:                  &kvPriorityClassHooks{},
	}
}

type kvPriorityClassHooks struct{}

func (h kvPriorityClassHooks) getFullCr(hc *hcov1beta1.HyperConverged) (client.Object, error) {
	return NewKubeVirtPriorityClass(hc), nil
}
func (h kvPriorityClassHooks) getEmptyCr() client.Object                               { return &schedulingv1.PriorityClass{} }
func (h kvPriorityClassHooks) validate() error                                         { return nil }
func (h kvPriorityClassHooks) postFound(_ *common.HcoRequest, _ runtime.Object) error  { return nil }
func (h kvPriorityClassHooks) getConditions(_ runtime.Object) []conditionsv1.Condition { return nil }
func (h kvPriorityClassHooks) checkComponentVersion(_ runtime.Object) bool             { return true }
func (h kvPriorityClassHooks) getObjectMeta(cr runtime.Object) *metav1.ObjectMeta {
	return &cr.(*schedulingv1.PriorityClass).ObjectMeta
}
func (h kvPriorityClassHooks) reset() { /* no implementation */ }

func (h *kvPriorityClassHooks) updateCr(req *common.HcoRequest, Client client.Client, exists runtime.Object, required runtime.Object) (bool, bool, error) {
	pc, ok1 := required.(*schedulingv1.PriorityClass)
	found, ok2 := exists.(*schedulingv1.PriorityClass)
	if !ok1 || !ok2 {
		return false, false, errors.New("can't convert to PriorityClass")
	}

	// at this point we found the object in the cache and we check if something was changed
	if (pc.Name == found.Name) && (pc.Value == found.Value) &&
		(pc.Description == found.Description) && reflect.DeepEqual(pc.Labels, found.Labels) {
		return false, false, nil
	}

	if req.HCOTriggered {
		req.Logger.Info("Updating existing KubeVirt's Spec to new opinionated values")
	} else {
		req.Logger.Info("Reconciling an externally updated KubeVirt's Spec to its opinionated values")
	}

	// something was changed but since we can't patch a priority class object, we remove it
	err := Client.Delete(req.Ctx, found, &client.DeleteOptions{})
	if err != nil {
		return false, false, err
	}

	// create the new object
	err = Client.Create(req.Ctx, pc, &client.CreateOptions{})
	if err != nil {
		return false, false, err
	}

	return true, !req.HCOTriggered, nil
}

func NewKubeVirtPriorityClass(hc *hcov1beta1.HyperConverged) *schedulingv1.PriorityClass {
	return &schedulingv1.PriorityClass{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "scheduling.k8s.io/v1",
			Kind:       "PriorityClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "kubevirt-cluster-critical",
			Labels: getLabels(hc, hcoutil.AppComponentCompute),
		},
		// 1 billion is the highest value we can set
		// https://kubernetes.io/docs/concepts/configuration/pod-priority-preemption/#priorityclass
		Value:         1000000000,
		GlobalDefault: false,
		Description:   "This priority class should be used for KubeVirt core components only.",
	}
}

// translateKubeVirtConds translates list of KubeVirt conditions to a list of custom resource
// conditions.
func translateKubeVirtConds(orig []kubevirtv1.KubeVirtCondition) []conditionsv1.Condition {
	translated := make([]conditionsv1.Condition, len(orig))

	for i, origCond := range orig {
		translated[i] = conditionsv1.Condition{
			Type:    conditionsv1.ConditionType(origCond.Type),
			Status:  origCond.Status,
			Reason:  origCond.Reason,
			Message: origCond.Message,
		}
	}

	return translated
}

func NewKubeVirtConfigForCR(cr *hcov1beta1.HyperConverged, namespace string) *corev1.ConfigMap {
	fgs := getKvFeatureGateList(cr.Spec.FeatureGates)
	featureGates := strings.Join(fgs, ",")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubevirt-config",
			Labels:    getLabels(cr, hcoutil.AppComponentCompute),
			Namespace: namespace,
		},
		// only virtconfig.SmbiosConfigKey, virtconfig.MachineTypeKey, virtconfig.SELinuxLauncherTypeKey,
		// virtconfig.FeatureGatesKey and virtconfig.UseEmulationKey are going to be manipulated
		// and only on HCO upgrades.
		// virtconfig.MigrationsConfigKey is going to be removed if set in the past (only during upgrades).
		// TODO: This is going to change in the next HCO release where the whole configMap is going
		// to be continuously reconciled
		Data: map[string]string{
			FeatureGatesKey:        featureGates,
			SELinuxLauncherTypeKey: "virt_launcher.process",
			NetworkInterfaceKey:    kubevirtDefaultNetworkInterfaceValue,
		},
	}
	val, ok := os.LookupEnv(smbiosEnvName)
	if ok && val != "" {
		cm.Data[SmbiosConfigKey] = val
	}
	val, ok = os.LookupEnv(machineTypeEnvName)
	if ok && val != "" {
		cm.Data[MachineTypeKey] = val
	}
	val, ok = os.LookupEnv(kvmEmulationEnvName)
	if ok && val != "" {
		cm.Data[UseEmulationKey] = val
	}
	return cm
}

// get list of feature gates or KV FG list
func getKvFeatureGateList(fgs *hcov1beta1.HyperConvergedFeatureGates) []string {
	checks := getFeatureGateChecks(fgs)
	res := make([]string, 0, len(checks)+len(hardCodeKvFgs))
	res = append(res, hardCodeKvFgs...)

	for gate, check := range checks {
		if check() {
			res = append(res, gate)
		}
	}

	return res
}
