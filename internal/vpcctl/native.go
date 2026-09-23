package vpcctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"globalvpc.io/controller/integration/kube-ovn/destinationroute"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const nativeAPIVersion = "vpcctl.globalvpc.io/v1alpha1"
const nativePatchSHA256 = "d338f05a6c4b7fa538637ee78a155a4ab2361d3b2e67ef665f359feaad5d9912"

var digestImage = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)

type NativeSchema struct {
	Spec   map[string]any `json:"spec"`
	Status map[string]any `json:"status"`
}

// NativeBundle is a portable build output. It does not certify packet delivery.
type NativeBundle struct {
	APIVersion    string            `json:"apiVersion"`
	Kind          string            `json:"kind"`
	Target        string            `json:"target"`
	SourceCommit  string            `json:"sourceCommit"`
	PatchSHA256   string            `json:"patchSHA256"`
	Image         string            `json:"image"`
	Platform      string            `json:"platform"`
	Schema        NativeSchema      `json:"schema"`
	BaseImage     string            `json:"baseImage,omitempty"`
	BinarySHA256  string            `json:"binarySHA256,omitempty"`
	Qualification map[string]string `json:"qualification,omitempty"`
}

type nativePatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

type nativePlan struct {
	APIVersion            string                 `json:"apiVersion"`
	Kind                  string                 `json:"kind"`
	Bundle                NativeBundle           `json:"bundle"`
	Context               string                 `json:"context,omitempty"`
	ClusterUID            types.UID              `json:"clusterUID"`
	Namespace             string                 `json:"namespace"`
	Deployment            string                 `json:"deployment"`
	Container             string                 `json:"container"`
	CRDUID                types.UID              `json:"crdUID"`
	CRDVersion            string                 `json:"crdResourceVersion"`
	CRDSpec               map[string]any         `json:"originalCRDSpec"`
	DeploymentUID         types.UID              `json:"deploymentUID"`
	DeploymentVersion     string                 `json:"deploymentResourceVersion"`
	Template              corev1.PodTemplateSpec `json:"originalTemplate"`
	CRDPatch              []nativePatchOperation `json:"crdPatch"`
	DeploymentPatch       []nativePatchOperation `json:"deploymentPatch"`
	Owners                []string               `json:"owners,omitempty"`
	InstallBlockers       []string               `json:"installBlockers,omitempty"`
	OriginalRuntimeImages []string               `json:"originalRuntimeImages,omitempty"`
	Previous              *nativePlan            `json:"previousPlan,omitempty"`
}

func (a *App) runNative(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(a.Out, `Usage: vpcctl maintainer native <plan|install|upgrade|verify|drain|rollback|uninstall|status>

  plan --bundle bundle.json --out install-plan.json
       (or --release RELEASE_URL_OR_FILE --target v1.16.3)
       [--namespace kube-system --deployment kube-ovn-controller --container kube-ovn-controller]
       [--upgrade-from previous-plan.json]
  install --plan install-plan.json [--direct-ownership]
  upgrade --plan upgrade-plan.json [--direct-ownership]
  verify [--plan install-plan.json]
  drain --plan install-plan.json
  rollback --plan install-plan.json [--direct-ownership]
  uninstall --plan install-plan.json [--direct-ownership]
  status [--namespace kube-system --deployment kube-ovn-controller --container kube-ovn-controller]

Plan is read-only and writes a private, cluster-bound review artifact. Install
rechecks the plan, dry-runs both guarded patches, and waits for all native
controller replicas. Pause destination-route materializers until rollout ends.
For Helm/Flux/Argo ownership, update the owner's desired state using the plan;
--direct-ownership explicitly selects coordinated direct mutation instead.

Keep every private plan: it is the receipt used for retry, rollback, and uninstall.
Upgrade plans embed the preceding receipt. Rollback restores the preceding image;
uninstall follows the receipt chain to the stock baseline. Returning to stock
requires empty intent, current empty ACKs, and independent OVN route/BFD absence.
Drain only verifies cleanup; use the public API to delete tenant resources first.
Native acknowledgement establishes configuration, never packet reachability.`)
		return nil
	}
	if args[0] == "upgrade" || args[0] == "verify" || args[0] == "drain" || args[0] == "rollback" || args[0] == "uninstall" {
		return a.runNativeLifecycle(ctx, args, opts)
	}
	fs := flag.NewFlagSet("native "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.ErrOut)
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "kube-system"
	}
	deployment, container, bundleFile, outFile, planFile, previousFile := "kube-ovn-controller", "kube-ovn-controller", "", "", "", ""
	releaseRef, target := "", ""
	directOwnership := false
	switch args[0] {
	case "plan", "status":
		fs.StringVar(&namespace, "namespace", namespace, "Native controller namespace")
		fs.StringVar(&deployment, "deployment", deployment, "Existing native controller Deployment")
		fs.StringVar(&container, "container", container, "Native controller container")
		if args[0] == "plan" {
			fs.StringVar(&bundleFile, "bundle", "", "Native bundle JSON")
			fs.StringVar(&outFile, "out", "", "New private plan file (must not exist)")
			fs.StringVar(&previousFile, "upgrade-from", "", "Installed plan receipt to upgrade from (embedded in the new plan)")
			fs.StringVar(&releaseRef, "release", "", "Published release descriptor URL or file (alternative to --bundle)")
			fs.StringVar(&target, "target", "", "Native baseline target in the release (for example v1.16.3)")
		}
	case "install":
		fs.StringVar(&planFile, "plan", "", "Reviewed native install plan")
		fs.BoolVar(&directOwnership, "direct-ownership", false, "Explicitly coordinate direct patches to owner-managed resources")
	default:
		return fmt.Errorf("unknown native command %q; use vpcctl maintainer native --help", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if namespace == "" || deployment == "" || container == "" {
		return fmt.Errorf("namespace, deployment, and container must be nonempty")
	}
	switch args[0] {
	case "plan":
		if outFile == "" || (bundleFile == "") == (releaseRef == "") || (releaseRef != "" && target == "") || (bundleFile != "" && target != "") {
			return fmt.Errorf("native plan requires --out and either --bundle or --release with --target")
		}
		var bundle NativeBundle
		if bundleFile != "" {
			if err := nativeReadJSON(bundleFile, &bundle); err != nil {
				return err
			}
		} else {
			r, err := a.loadRelease(ctx, releaseRef)
			if err != nil {
				return err
			}
			var ok bool
			bundle, ok = r.NativeBundles[target]
			if !ok {
				return fmt.Errorf("release does not contain native target %q", target)
			}
		}
		if err := validateNativeBundle(bundle); err != nil {
			return err
		}
		c, err := a.client(opts)
		if err != nil {
			return err
		}
		var p nativePlan
		if previousFile != "" {
			var previous nativePlan
			if err := nativeReadJSON(previousFile, &previous); err != nil {
				return err
			}
			p, err = makeNativeUpgradePlan(ctx, c, opts, previous, bundle)
		} else {
			p, err = makeNativePlan(ctx, c, opts.Context, namespace, deployment, container, bundle)
		}
		if err != nil {
			return err
		}
		if err := nativeWritePrivateJSON(outFile, p); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Plan saved to %s (0600). Cluster UID: %s\n", outFile, p.ClusterUID)
		if p.Previous == nil {
			fmt.Fprintln(a.Out, "Schema change: add only Vpc spec/status.destinationRoutes.")
		} else {
			fmt.Fprintln(a.Out, "Compatible upgrade: retain the destination-routes.v1 schema and embed the previous plan receipt.")
		}
		fmt.Fprintf(a.Out, "Image change: replace %s/%s container %s image.\n", p.Namespace, p.Deployment, p.Container)
		fmt.Fprintf(a.Out, "Image: %s\n", p.Bundle.Image)
		if len(p.Owners) > 0 {
			fmt.Fprintf(a.Out, "Detected resource managers: %s. Update their desired state with this plan before a coordinated rollout.\n", strings.Join(p.Owners, ", "))
		}
		for _, blocker := range p.InstallBlockers {
			fmt.Fprintf(a.Out, "Direct install blocked: %s\n", blocker)
		}
		fmt.Fprintf(a.Out, "Real OVN/northd qualification: %s; packet forwarding: %s. This plan does not establish either.\n", nativeQualification(bundle, "realOVNNorthd"), nativeQualification(bundle, "packetForwarding"))
		fmt.Fprintln(a.Out, "Keep destination-route materializers paused until all native replicas finish rollout. Keep this private plan for retry, rollback, and uninstall.")
		return nil
	case "install":
		if planFile == "" {
			return fmt.Errorf("native install requires --plan")
		}
		var p nativePlan
		if err := nativeReadJSON(planFile, &p); err != nil {
			return err
		}
		if p.Previous != nil {
			return fmt.Errorf("upgrade plan requires maintainer native upgrade --plan")
		}
		c, err := a.client(opts)
		if err != nil {
			return err
		}
		return a.installNativePlan(ctx, c, p, opts, directOwnership)
	default:
		c, err := a.client(opts)
		if err != nil {
			return err
		}
		return a.nativeStatus(ctx, c, namespace, deployment, container, opts)
	}
}

func nativeQualification(b NativeBundle, key string) string {
	if value := b.Qualification[key]; value != "" {
		return value
	}
	return "not recorded"
}

func validateNativeBundle(b NativeBundle) error {
	commits := map[string]string{"v1.16.3": "98af25ffae49193a8dc16bbc39bd8ca4110ec367", "v1.16.4": "a9296ef2a37c6519bc0ecb798082ce139c72f8eb"}
	if b.APIVersion != nativeAPIVersion || b.Kind != "NativeBundle" {
		return fmt.Errorf("unsupported native bundle apiVersion or kind")
	}
	if commits[b.Target] == "" || b.SourceCommit != commits[b.Target] || b.PatchSHA256 != nativePatchSHA256 {
		return fmt.Errorf("bundle target/source/patch does not match a supported pinned integration")
	}
	if b.Platform != "linux/amd64" {
		return fmt.Errorf("native bundle platform must be linux/amd64")
	}
	if !digestImage.MatchString(b.Image) {
		return fmt.Errorf("native image must use an immutable @sha256 digest")
	}
	if b.BaseImage != "" && !digestImage.MatchString(b.BaseImage) {
		return fmt.Errorf("native base image must use an immutable @sha256 digest")
	}
	if !nativeJSONEqual(b.Schema, nativeSchema()) {
		return fmt.Errorf("bundle schema differs from the pinned destination-routes.v1 contract")
	}
	return nil
}

func nativeCRD() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apiextensions.k8s.io/v1")
	u.SetKind("CustomResourceDefinition")
	u.SetName("vpcs.kubeovn.io")
	return u
}

func nativeVPCList(ctx context.Context, c client.Client) (*unstructured.UnstructuredList, error) {
	l := &unstructured.UnstructuredList{}
	l.SetAPIVersion("kubeovn.io/v1")
	l.SetKind("VpcList")
	if err := c.List(ctx, l); err != nil {
		return nil, fmt.Errorf("list native Vpcs: %w", err)
	}
	return l, nil
}

func nativeNoIntent(ctx context.Context, c client.Client) error {
	l, err := nativeVPCList(ctx, c)
	if err != nil {
		return err
	}
	for _, vpc := range l.Items {
		routes, _, err := unstructured.NestedSlice(vpc.Object, "spec", "destinationRoutes")
		if err != nil {
			return fmt.Errorf("Vpc %s has malformed destinationRoutes: %w", vpc.GetName(), err)
		}
		if len(routes) != 0 {
			return fmt.Errorf("Vpc %s has active destinationRoutes; first-install workflow refuses mutation", vpc.GetName())
		}
	}
	return nil
}

func nativeClusterUID(ctx context.Context, c client.Client) (types.UID, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		return "", fmt.Errorf("read cluster identity: %w", err)
	}
	if ns.UID == "" {
		return "", fmt.Errorf("kube-system Namespace has no UID")
	}
	return ns.UID, nil
}

func nativeCRDProperties(crd *unstructured.Unstructured) (map[string]any, error) {
	conversion, _, err := unstructured.NestedString(crd.Object, "spec", "conversion", "strategy")
	if err != nil || (conversion != "" && conversion != "None") {
		return nil, fmt.Errorf("unsupported Vpc CRD conversion; require strategy None")
	}
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || len(versions) != 1 {
		return nil, fmt.Errorf("unsupported Vpc CRD layout: require a single served/storage v1 schema")
	}
	v, ok := versions[0].(map[string]any)
	if !ok || v["name"] != "v1" || v["served"] != true || v["storage"] != true {
		return nil, fmt.Errorf("unsupported Vpc CRD version layout")
	}
	props, found, err := unstructured.NestedMap(v, "schema", "openAPIV3Schema", "properties")
	if err != nil || !found {
		return nil, fmt.Errorf("Vpc CRD structural schema is missing")
	}
	for _, section := range []string{"spec", "status"} {
		if _, found, err := unstructured.NestedMap(props, section, "properties"); err != nil || !found {
			return nil, fmt.Errorf("Vpc CRD %s properties are missing", section)
		}
	}
	if _, found, err := unstructured.NestedMap(props, "spec", "properties", "bfdPort"); err != nil || !found {
		return nil, fmt.Errorf("Vpc CRD is missing the required existing spec.bfdPort schema")
	}
	return props, nil
}

func nativeContainerIndex(d *appsv1.Deployment, name string) (int, error) {
	for i, c := range d.Spec.Template.Spec.Containers {
		if c.Name == name {
			return i, nil
		}
	}
	return -1, fmt.Errorf("container %q is absent from Deployment %s/%s", name, d.Namespace, d.Name)
}

func validateNativeDeployment(d *appsv1.Deployment, container string) (int, error) {
	if d.UID == "" || d.ResourceVersion == "" {
		return -1, fmt.Errorf("native Deployment must have UID and resourceVersion")
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || d.Spec.Paused {
		return -1, fmt.Errorf("native Deployment must have positive explicit replicas and must not be paused")
	}
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels)+len(d.Spec.Selector.MatchExpressions) == 0 {
		return -1, fmt.Errorf("native Deployment must have a nonempty selector")
	}
	i, err := nativeContainerIndex(d, container)
	if err != nil {
		return -1, err
	}
	native := d.Spec.Template.Spec.Containers[i]
	entrypoint := ""
	if len(native.Command) > 0 {
		entrypoint = native.Command[0]
	} else if len(native.Args) > 0 {
		entrypoint = native.Args[0]
	}
	if entrypoint != "/kube-ovn/start-controller.sh" && entrypoint != "/kube-ovn/kube-ovn-controller" && entrypoint != "start-controller.sh" && entrypoint != "kube-ovn-controller" {
		return -1, fmt.Errorf("unsupported native container command; require the known /kube-ovn controller layout")
	}
	for _, mount := range native.VolumeMounts {
		p := path.Clean(mount.MountPath)
		if p == "/" || p == "/kube-ovn" || p == "/kube-ovn/kube-ovn-controller" || p == "/kube-ovn/start-controller.sh" {
			return -1, fmt.Errorf("volume mount %q hides the image controller executable or entrypoint; resolve it before planning", mount.MountPath)
		}
	}
	return i, nil
}

func nativeManagers(obj metav1.Object) []string {
	seen := map[string]bool{}
	for key, value := range obj.GetLabels() {
		if strings.Contains(key, "helm") || strings.Contains(key, "fluxcd") || strings.Contains(key, "argocd") || (key == "app.kubernetes.io/managed-by" && value != "vpcctl") {
			seen[key+"="+value] = true
		}
	}
	for key := range obj.GetAnnotations() {
		if strings.Contains(key, "helm.sh/") || strings.Contains(key, "argocd.argoproj.io/") {
			seen[key] = true
		}
	}
	for _, entry := range obj.GetManagedFields() {
		m := strings.ToLower(entry.Manager)
		if strings.Contains(m, "helm") || strings.Contains(m, "flux") || strings.Contains(m, "argocd") || strings.Contains(m, "kustomize-controller") {
			seen["manager="+entry.Manager] = true
		}
	}
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Controller != nil && *owner.Controller {
			seen["owner="+owner.Kind+"/"+owner.Name] = true
		}
	}
	result := make([]string, 0, len(seen))
	for owner := range seen {
		result = append(result, owner)
	}
	sort.Strings(result)
	return result
}

func nativeImageReference(image string) string {
	parts := strings.SplitN(image, "@", 2)
	if len(parts) != 2 {
		return image
	}
	repository := parts[0]
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	return repository + "@" + parts[1]
}

// A mutable tag is acceptable only when every owned, running native Pod proves
// the immutable base digest in the bundle. Index/platform digest differences
// remain unresolved rather than being guessed equal.
func nativeImagePreflight(ctx context.Context, c client.Client, d *appsv1.Deployment, container string, bundle NativeBundle) ([]string, []string) {
	i, err := nativeContainerIndex(d, container)
	if err != nil {
		return []string{err.Error()}, nil
	}
	original := d.Spec.Template.Spec.Containers[i].Image
	if digestImage.MatchString(original) {
		if bundle.BaseImage != "" && nativeImageReference(original) != nativeImageReference(bundle.BaseImage) {
			return []string{"current native image differs from bundle.baseImage; use a bundle built from this exact native distribution"}, nil
		}
		return nil, []string{nativeImageReference(original)}
	}
	blocked := "current native image is mutable and its running digest cannot be proven equal to bundle.baseImage; verify matching platform digests (an index digest is not a platform digest), or pin the verified original image in its owner and regenerate the plan"
	if bundle.BaseImage == "" {
		return []string{blocked}, nil
	}
	ready, _, pods, err := nativeRolloutSnapshot(ctx, c, d, container, original)
	if err != nil || !ready {
		return []string{blocked}, nil
	}
	expected := strings.SplitN(bundle.BaseImage, "@", 2)[1]
	identities := map[string]bool{}
	for _, pod := range pods {
		matched := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != container {
				continue
			}
			id := status.ImageID
			if marker := strings.Index(id, "://"); marker >= 0 {
				id = id[marker+3:]
			}
			if id == expected || (digestImage.MatchString(id) && nativeImageReference(id) == nativeImageReference(bundle.BaseImage)) {
				matched = true
				identities[nativeImageReference(bundle.BaseImage)] = true
			}
		}
		if !matched {
			return []string{blocked}, nil
		}
	}
	result := make([]string, 0, len(identities))
	for id := range identities {
		result = append(result, id)
	}
	sort.Strings(result)
	return nil, result
}

func nativeExpectedState(p nativePlan) (map[string]any, corev1.PodTemplateSpec) {
	// JSON round-tripping keeps snapshots independent and normalizes number types.
	raw, _ := json.Marshal(p.CRDSpec)
	var spec map[string]any
	_ = json.Unmarshal(raw, &spec)
	versions := spec["versions"].([]any)
	v := versions[0].(map[string]any)
	_ = unstructured.SetNestedMap(v, p.Bundle.Schema.Spec, "schema", "openAPIV3Schema", "properties", "spec", "properties", "destinationRoutes")
	_ = unstructured.SetNestedMap(v, p.Bundle.Schema.Status, "schema", "openAPIV3Schema", "properties", "status", "properties", "destinationRoutes")
	template := *p.Template.DeepCopy()
	for i := range template.Spec.Containers {
		if template.Spec.Containers[i].Name == p.Container {
			template.Spec.Containers[i].Image = p.Bundle.Image
		}
	}
	return spec, template
}

func makeNativePlan(ctx context.Context, c client.Client, contextName, namespace, deployment, container string, bundle NativeBundle) (nativePlan, error) {
	p := nativePlan{APIVersion: nativeAPIVersion, Kind: "NativeInstallPlan", Bundle: bundle, Context: contextName, Namespace: namespace, Deployment: deployment, Container: container}
	if err := validateNativeBundle(bundle); err != nil {
		return p, err
	}
	uid, err := nativeClusterUID(ctx, c)
	if err != nil {
		return p, err
	}
	p.ClusterUID = uid
	if err := nativeNoIntent(ctx, c); err != nil {
		return p, err
	}
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return p, fmt.Errorf("get Vpc CRD: %w", err)
	}
	if crd.GetUID() == "" || crd.GetResourceVersion() == "" {
		return p, fmt.Errorf("Vpc CRD must have UID and resourceVersion")
	}
	props, err := nativeCRDProperties(crd)
	if err != nil {
		return p, err
	}
	for _, section := range []string{"spec", "status"} {
		if _, exists, _ := unstructured.NestedFieldNoCopy(props, section, "properties", "destinationRoutes"); exists {
			return p, fmt.Errorf("Vpc CRD %s.destinationRoutes already exists; first-install workflow does not upgrade or replace custom schemas", section)
		}
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, &d); err != nil {
		return p, fmt.Errorf("get native Deployment: %w", err)
	}
	i, err := validateNativeDeployment(&d, container)
	if err != nil {
		return p, err
	}
	p.CRDUID = crd.GetUID()
	p.CRDVersion = crd.GetResourceVersion()
	p.CRDSpec, _, _ = unstructured.NestedMap(crd.Object, "spec")
	p.DeploymentUID = d.UID
	p.DeploymentVersion = d.ResourceVersion
	p.Template = *d.Spec.Template.DeepCopy()
	p.Owners = append(nativeManagers(crd), nativeManagers(&d)...)
	sort.Strings(p.Owners)
	p.InstallBlockers, p.OriginalRuntimeImages = nativeImagePreflight(ctx, c, &d, container, bundle)
	p.CRDPatch, p.DeploymentPatch = nativePlanPatches(p, i)
	return p, nil
}

func nativePlanPatches(p nativePlan, i int) ([]nativePatchOperation, []nativePatchOperation) {
	crd := []nativePatchOperation{{"test", "/metadata/uid", p.CRDUID}, {"test", "/metadata/resourceVersion", p.CRDVersion}}
	if p.Previous == nil {
		for _, section := range []string{"spec", "status"} {
			fragment := p.Bundle.Schema.Spec
			if section == "status" {
				fragment = p.Bundle.Schema.Status
			}
			crd = append(crd, nativePatchOperation{"add", "/spec/versions/0/schema/openAPIV3Schema/properties/" + section + "/properties/destinationRoutes", fragment})
		}
	}
	deployment := []nativePatchOperation{{"test", "/metadata/uid", p.DeploymentUID}, {"test", "/metadata/resourceVersion", p.DeploymentVersion}, {"test", "/spec/template", p.Template}, {"replace", fmt.Sprintf("/spec/template/spec/containers/%d/image", i), p.Bundle.Image}}
	return crd, deployment
}

func validateNativePlan(p nativePlan) error {
	return validateNativePlanDepth(p, 0)
}

func validateNativePlanDepth(p nativePlan, depth int) error {
	if depth > 32 {
		return fmt.Errorf("native receipt chain exceeds 32 upgrades")
	}
	if p.APIVersion != nativeAPIVersion || p.Kind != "NativeInstallPlan" || p.ClusterUID == "" || p.Namespace == "" || p.Deployment == "" || p.Container == "" || p.CRDUID == "" || p.CRDVersion == "" || p.DeploymentUID == "" || p.DeploymentVersion == "" {
		return fmt.Errorf("invalid or incomplete native install plan")
	}
	if err := validateNativeBundle(p.Bundle); err != nil {
		return err
	}
	baseline := nativeCRD()
	baseline.Object["spec"] = p.CRDSpec
	props, err := nativeCRDProperties(baseline)
	if err != nil {
		return fmt.Errorf("invalid original CRD snapshot in plan: %w", err)
	}
	if p.Previous == nil {
		for _, section := range []string{"spec", "status"} {
			if _, exists, _ := unstructured.NestedFieldNoCopy(props, section, "properties", "destinationRoutes"); exists {
				return fmt.Errorf("plan original CRD snapshot already contains extension fields")
			}
		}
	} else {
		if err := validateNativePlanDepth(*p.Previous, depth+1); err != nil {
			return fmt.Errorf("invalid previous plan: %w", err)
		}
		previous := p.Previous
		previousSpec, previousTemplate := nativeExpectedState(*previous)
		if p.ClusterUID != previous.ClusterUID || p.CRDUID != previous.CRDUID || p.DeploymentUID != previous.DeploymentUID || p.Namespace != previous.Namespace || p.Deployment != previous.Deployment || p.Container != previous.Container || !nativeJSONEqual(p.CRDSpec, previousSpec) || !nativeJSONEqual(p.Template, previousTemplate) {
			return fmt.Errorf("upgrade receipt does not continue the previous exact installed state")
		}
		if p.Bundle.Target != previous.Bundle.Target || nativeImageReference(p.Bundle.BaseImage) != nativeImageReference(previous.Bundle.BaseImage) {
			return fmt.Errorf("native upgrade must retain the same upstream target and base image; upgrade the full upstream Kube-OVN installation separately")
		}
		if len(p.OriginalRuntimeImages) != 1 || p.OriginalRuntimeImages[0] != previous.Bundle.Image {
			return fmt.Errorf("upgrade receipt original immutable image differs from its previous bundle")
		}
	}
	i := -1
	for index, c := range p.Template.Spec.Containers {
		if c.Name == p.Container {
			i = index
		}
	}
	if i < 0 {
		return fmt.Errorf("plan original template does not contain native container")
	}
	cp, dp := nativePlanPatches(p, i)
	if !nativeJSONEqual(cp, p.CRDPatch) || !nativeJSONEqual(dp, p.DeploymentPatch) {
		return fmt.Errorf("plan patches differ from the narrow supported image/schema changes; regenerate the plan")
	}
	return nil
}

func (a *App) installNativePlan(ctx context.Context, c client.Client, p nativePlan, opts Options, direct bool) error {
	if err := validateNativePlan(p); err != nil {
		return err
	}
	uid, err := nativeClusterUID(ctx, c)
	if err != nil {
		return err
	}
	if uid != p.ClusterUID {
		return fmt.Errorf("wrong cluster: kube-system UID differs from the reviewed plan")
	}
	if p.Context != "" && opts.Context != "" && p.Context != opts.Context {
		return fmt.Errorf("context differs from the reviewed plan")
	}
	if err := nativeInstallIntentPreflight(ctx, c, p); err != nil {
		return fmt.Errorf("installation preflight: %w", err)
	}
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	var deployment appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Deployment}, &deployment); err != nil {
		return err
	}
	if crd.GetUID() != p.CRDUID || deployment.UID != p.DeploymentUID {
		return fmt.Errorf("stale plan: CRD or Deployment UID changed")
	}
	i, err := validateNativeDeployment(&deployment, p.Container)
	if err != nil {
		return err
	}
	if _, err := nativeCRDProperties(crd); err != nil {
		return err
	}
	expectedSpec, expectedTemplate := nativeExpectedState(p)
	currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	schemaApplied := nativeJSONEqual(currentSpec, expectedSpec)
	imageApplied := nativeJSONEqual(deployment.Spec.Template, expectedTemplate)
	if !schemaApplied && !nativeJSONEqual(currentSpec, p.CRDSpec) {
		return fmt.Errorf("stale plan: CRD differs from both the original and exact planned schema")
	}
	if !imageApplied && !nativeJSONEqual(deployment.Spec.Template, p.Template) {
		return fmt.Errorf("stale plan: Deployment template differs from both the original and exact planned template")
	}
	if !schemaApplied && imageApplied {
		return fmt.Errorf("inconsistent partial install: target image exists without the planned schema")
	}
	if !schemaApplied && (crd.GetResourceVersion() != p.CRDVersion || deployment.ResourceVersion != p.DeploymentVersion) {
		return fmt.Errorf("stale plan: CRD or Deployment resourceVersion changed; create and review a fresh plan")
	}
	owners := append(nativeManagers(crd), nativeManagers(&deployment)...)
	if len(owners) > 0 && !direct {
		return fmt.Errorf("owner-managed resources detected (%s): update their desired state from the plan; coordinated direct mutation requires --direct-ownership", strings.Join(owners, ", "))
	}
	if !imageApplied && p.Previous == nil {
		blockers, _ := nativeImagePreflight(ctx, c, &deployment, p.Container, p.Bundle)
		if len(blockers) > 0 {
			return fmt.Errorf("installation blocked: %s", strings.Join(blockers, "; "))
		}
	}
	if schemaApplied {
		fmt.Fprintln(a.Out, "Resuming the same plan: exact planned schema and original resource identities verified.")
	}
	// Resource versions may advance after the schema step. Resume permits only
	// the exact original template or the exact intended image-only template.
	working := p
	working.CRDVersion = crd.GetResourceVersion()
	working.DeploymentVersion = deployment.ResourceVersion
	_, deploymentOperations := nativePlanPatches(working, i)
	crdPatch, _ := json.Marshal(p.CRDPatch)
	deploymentPatch, _ := json.Marshal(deploymentOperations)
	for _, dryRun := range []bool{true, false} {
		patchOpts := []client.PatchOption{}
		if dryRun {
			patchOpts = append(patchOpts, client.DryRunAll)
		}
		if err := nativeInstallIntentPreflight(ctx, c, p); err != nil {
			if schemaApplied {
				return fmt.Errorf("PARTIAL INSTALL: planned schema exists; stop materializers before resuming: %w", err)
			}
			return err
		}
		if !schemaApplied {
			object := nativeCRD()
			if err := c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, crdPatch), patchOpts...); err != nil {
				return fmt.Errorf("CRD patch (dry-run=%t) failed; controller image was not changed: %w", dryRun, err)
			}
			if !dryRun {
				schemaApplied = true
				fmt.Fprintln(a.Out, "Applied the two destinationRoutes CRD properties.")
			}
		}
		if !imageApplied {
			if err := nativeInstallIntentPreflight(ctx, c, p); err != nil {
				if !dryRun {
					return fmt.Errorf("PARTIAL INSTALL: schema applied, image unchanged; stop materializers and resume this plan: %w", err)
				}
				return err
			}
			object := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Deployment}}
			if err := c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, deploymentPatch), patchOpts...); err != nil {
				if !dryRun {
					return fmt.Errorf("PARTIAL INSTALL: schema applied, Deployment image patch failed; inspect native status and rerun install with the same plan after resolving the error; no automatic rollback attempted: %w", err)
				}
				return fmt.Errorf("Deployment dry-run failed; no new changes applied: %w", err)
			}
			if !nativeJSONEqual(object.Spec.Template, expectedTemplate) {
				if !dryRun {
					return fmt.Errorf("PARTIAL INSTALL: native Deployment admission changed fields beyond the planned image; inspect native status; no automatic rollback attempted")
				}
				return fmt.Errorf("native Deployment dry-run admission changed fields beyond the planned image; no new changes applied")
			}
			if !dryRun {
				expectedTemplate = *object.Spec.Template.DeepCopy()
			}
		}
	}
	fmt.Fprintln(a.Out, "Controller image applied; waiting for all desired replicas and owned Pods.")
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := nativeWaitRollout(waitCtx, c, p.Namespace, p.Deployment, p.Container, p.DeploymentUID, p.Bundle.Image, expectedTemplate); err != nil {
		return fmt.Errorf("PARTIAL INSTALL: schema and image applied, but rollout is unverified; keep materializers paused and inspect native status; rerun install with the same plan to resume; no automatic rollback attempted: %w", err)
	}
	finalCRD := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(finalCRD), finalCRD); err != nil {
		return fmt.Errorf("PARTIAL INSTALL: rollout completed but final schema verification failed: %w", err)
	}
	finalSpec, _, _ := unstructured.NestedMap(finalCRD.Object, "spec")
	if finalCRD.GetUID() != p.CRDUID || !nativeJSONEqual(finalSpec, expectedSpec) {
		return fmt.Errorf("PARTIAL INSTALL: Vpc CRD drifted during rollout; inspect native status before enabling intent")
	}
	fmt.Fprintln(a.Out, "Native controller rollout complete. Verify native ACK and tenant packets after enabling intent; rollout alone does not qualify forwarding.")
	return nil
}

func nativeWaitRollout(ctx context.Context, c client.Client, namespace, deployment, container string, uid types.UID, image string, expectedTemplate corev1.PodTemplateSpec) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := "deployment not observed"
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var d appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, &d); err != nil {
			return err
		}
		if d.UID != uid {
			return fmt.Errorf("Deployment was replaced during rollout")
		}
		if !nativeJSONEqual(d.Spec.Template, expectedTemplate) {
			return fmt.Errorf("Deployment template drifted during rollout")
		}
		ready, reason, err := nativeRolloutReady(ctx, c, &d, container, image)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		last = reason
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func nativeRolloutReady(ctx context.Context, c client.Client, d *appsv1.Deployment, container, image string) (bool, string, error) {
	ready, reason, _, err := nativeRolloutSnapshot(ctx, c, d, container, image)
	return ready, reason, err
}

func nativeRolloutSnapshot(ctx context.Context, c client.Client, d *appsv1.Deployment, container, image string) (bool, string, []corev1.Pod, error) {
	i, err := nativeContainerIndex(d, container)
	if err != nil {
		return false, "", nil, err
	}
	if d.Spec.Template.Spec.Containers[i].Image != image {
		return false, "", nil, fmt.Errorf("Deployment image changed during rollout")
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 {
		return false, "Deployment has no desired replicas", nil, nil
	}
	n := *d.Spec.Replicas
	if d.Spec.Selector == nil {
		return false, "", nil, fmt.Errorf("Deployment selector missing")
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return false, "", nil, err
	}
	var sets appsv1.ReplicaSetList
	if err := c.List(ctx, &sets, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, "", nil, err
	}
	owned := map[types.UID]bool{}
	for _, set := range sets.Items {
		for _, owner := range set.OwnerReferences {
			if owner.UID == d.UID && owner.Kind == "Deployment" && owner.Controller != nil && *owner.Controller {
				owned[set.UID] = true
			}
		}
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, "", nil, err
	}
	verified := make([]corev1.Pod, 0, len(pods.Items))
	foreign := false
	for _, pod := range pods.Items {
		isOwned := false
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "ReplicaSet" && owner.Controller != nil && *owner.Controller && owned[owner.UID] {
				isOwned = true
			}
		}
		if isOwned {
			verified = append(verified, pod)
		} else {
			foreign = true
		}
	}
	if foreign {
		return false, "selector includes a Pod outside this Deployment owner chain", verified, nil
	}
	if int32(len(verified)) != n {
		return false, "waiting for exactly the desired Pods (including removal of old Pods)", verified, nil
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.Replicas != n || d.Status.UpdatedReplicas != n || d.Status.ReadyReplicas != n || d.Status.AvailableReplicas != n || d.Status.UnavailableReplicas != 0 {
		return false, "waiting for Deployment generation and all desired replicas", verified, nil
	}
	for _, pod := range verified {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			return false, "waiting for running, non-terminating Pods", verified, nil
		}
		matched := false
		for _, ctr := range pod.Spec.Containers {
			if ctr.Name == container && ctr.Image == image {
				matched = true
			}
		}
		ready := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == container && status.Ready && status.State.Running != nil {
				ready = true
			}
		}
		podReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				podReady = true
			}
		}
		if !matched || !ready || !podReady {
			return false, "waiting for every Pod to run the expected image and be Ready", verified, nil
		}
	}
	return true, "all desired replicas and owned Pods are Ready on the expected image", verified, nil
}

type nativePodReport struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Image   string `json:"image"`
	ImageID string `json:"imageID,omitempty"`
	Ready   bool   `json:"ready"`
}

type nativeACKReport struct {
	Name         string   `json:"name"`
	Routes       int      `json:"routes"`
	State        string   `json:"state"`
	ExpectedHash string   `json:"expectedHash,omitempty"`
	Problems     []string `json:"problems,omitempty"`
}

func nativeACK(vpc unstructured.Unstructured) nativeACKReport {
	r := nativeACKReport{Name: vpc.GetName(), State: "not-configured"}
	routes, _, err := unstructured.NestedSlice(vpc.Object, "spec", "destinationRoutes")
	if err != nil {
		r.State = "mismatch"
		r.Problems = []string{err.Error()}
		return r
	}
	r.Routes = len(routes)
	status, exists, err := unstructured.NestedMap(vpc.Object, "status", "destinationRoutes")
	if err != nil {
		r.State = "mismatch"
		r.Problems = []string{err.Error()}
		return r
	}
	if len(routes) == 0 && !exists {
		return r
	}
	data, _ := json.Marshal(routes)
	var intents []destinationroute.Intent
	if err := json.Unmarshal(data, &intents); err != nil {
		r.State = "mismatch"
		r.Problems = []string{"malformed route intent"}
		return r
	}
	compiled, err := destinationroute.Compile(intents)
	if err != nil {
		r.State = "mismatch"
		r.Problems = []string{err.Error()}
		return r
	}
	r.ExpectedHash = compiled.Hash
	if status["capability"] != destinationroute.Capability {
		r.Problems = append(r.Problems, "capability is missing or unsupported")
	}
	if status["ready"] != true {
		r.Problems = append(r.Problems, "ready is not true")
	}
	if status["appliedHash"] != compiled.Hash {
		r.Problems = append(r.Problems, "appliedHash differs from canonical intent")
	}
	gen, _, _ := unstructured.NestedInt64(vpc.Object, "status", "destinationRoutes", "observedGeneration")
	if gen != vpc.GetGeneration() {
		r.Problems = append(r.Problems, "observedGeneration differs from current generation")
	}
	r.State = "acknowledged"
	if len(r.Problems) > 0 {
		r.State = "mismatch"
	}
	return r
}

func (a *App) nativeStatus(ctx context.Context, c client.Client, namespace, deployment, container string, opts Options) error {
	uid, err := nativeClusterUID(ctx, c)
	if err != nil {
		return err
	}
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	props, err := nativeCRDProperties(crd)
	if err != nil {
		return err
	}
	spec, sp, _ := unstructured.NestedMap(props, "spec", "properties", "destinationRoutes")
	status, st, _ := unstructured.NestedMap(props, "status", "properties", "destinationRoutes")
	schemaState := "absent"
	if sp || st {
		schemaState = "mismatched"
		if nativeJSONEqual(NativeSchema{Spec: spec, Status: status}, nativeSchema()) {
			schemaState = "supported"
		}
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, &d); err != nil {
		return err
	}
	i, err := nativeContainerIndex(&d, container)
	if err != nil {
		return err
	}
	image := d.Spec.Template.Spec.Containers[i].Image
	ready, reason, pods, err := nativeRolloutSnapshot(ctx, c, &d, container, image)
	if err != nil {
		return err
	}
	vpcs, err := nativeVPCList(ctx, c)
	if err != nil {
		return err
	}
	reports := make([]nativeACKReport, 0, len(vpcs.Items))
	for _, vpc := range vpcs.Items {
		reports = append(reports, nativeACK(vpc))
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Name < reports[j].Name })
	podReports := make([]nativePodReport, 0, len(pods))
	for _, pod := range pods {
		r := nativePodReport{Name: pod.Name, Phase: string(pod.Status.Phase)}
		for _, ctr := range pod.Spec.Containers {
			if ctr.Name == container {
				r.Image = ctr.Image
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == container {
				r.ImageID = status.ImageID
				r.Ready = status.Ready
			}
		}
		podReports = append(podReports, r)
	}
	sort.Slice(podReports, func(i, j int) bool { return podReports[i].Name < podReports[j].Name })
	report := struct {
		ClusterUID    types.UID         `json:"clusterUID"`
		Schema        string            `json:"schema"`
		Deployment    string            `json:"deployment"`
		Image         string            `json:"desiredImage"`
		RolloutReady  bool              `json:"rolloutReady"`
		RolloutReason string            `json:"rolloutReason"`
		VPCs          []nativeACKReport `json:"vpcs"`
		Pods          []nativePodReport `json:"pods"`
		Boundary      string            `json:"boundary"`
	}{uid, schemaState, namespace + "/" + deployment, image, ready, reason, reports, podReports, "Schema and rollout do not prove native capability; matching native ACK confirms configuration, not BFD or packet delivery."}
	if opts.Output == "json" || opts.Output == "yaml" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if opts.Output == "yaml" {
			data, err = yaml.JSONToYAML(data)
			if err != nil {
				return err
			}
		}
		_, err = a.Out.Write(data)
		return err
	}
	fmt.Fprintf(a.Out, "Cluster UID: %s\nNative schema: %s\nDeployment: %s\nDesired image: %s\nRollout ready: %t (%s)\n", uid, schemaState, report.Deployment, image, ready, reason)
	for _, pod := range podReports {
		fmt.Fprintf(a.Out, "Pod %s: phase=%s ready=%t imageID=%s\n", pod.Name, pod.Phase, pod.Ready, pod.ImageID)
	}
	for _, r := range reports {
		fmt.Fprintf(a.Out, "Vpc %s: %s (%d destinations)", r.Name, r.State, r.Routes)
		if len(r.Problems) > 0 {
			fmt.Fprintf(a.Out, " — %s", strings.Join(r.Problems, "; "))
		}
		fmt.Fprintln(a.Out)
	}
	fmt.Fprintln(a.Out, report.Boundary)
	return nil
}

func nativeReadJSON(filename string, out any) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("read %s: %w", filename, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%s must contain exactly one JSON object", filename)
	}
	return nil
}

func nativeWritePrivateJSON(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func nativeJSONEqual(a, b any) bool {
	aa, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	// A persisted patch's Value decodes as a map even when it was originally a
	// typed PodTemplateSpec. Object key order must not change plan validity.
	var av, bv any
	ad := json.NewDecoder(strings.NewReader(string(aa)))
	bd := json.NewDecoder(strings.NewReader(string(bb)))
	ad.UseNumber()
	bd.UseNumber()
	return ad.Decode(&av) == nil && bd.Decode(&bv) == nil && reflect.DeepEqual(av, bv)
}
