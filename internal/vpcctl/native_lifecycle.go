package vpcctl

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nativeOVNOptions identifies the existing OVN NB client inside the cluster.
// No host-side kubectl, shell, or OVN installation is used.
type nativeOVNOptions struct {
	Namespace, Deployment, Container, Database string
}

func defaultNativeOVNOptions() nativeOVNOptions {
	return nativeOVNOptions{"kube-system", "ovn-central", "ovn-central", "unix:/run/ovn/ovnnb_db.sock"}
}

func (a *App) runNativeLifecycle(ctx context.Context, args []string, opts Options) error {
	fs := flag.NewFlagSet("native "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.ErrOut)
	planFile := fs.String("plan", "", "Private installed plan receipt")
	direct := fs.Bool("direct-ownership", false, "Explicitly coordinate direct patches to owner-managed resources")
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "kube-system"
	}
	deployment, container := "kube-ovn-controller", "kube-ovn-controller"
	if args[0] == "verify" {
		fs.StringVar(&namespace, "namespace", namespace, "Native controller namespace (without --plan)")
		fs.StringVar(&deployment, "deployment", deployment, "Native controller Deployment (without --plan)")
		fs.StringVar(&container, "container", container, "Native controller container (without --plan)")
	}
	ovn := defaultNativeOVNOptions()
	if args[0] == "drain" || args[0] == "rollback" || args[0] == "uninstall" {
		fs.StringVar(&ovn.Namespace, "ovn-namespace", ovn.Namespace, "Existing OVN NB client namespace")
		fs.StringVar(&ovn.Deployment, "ovn-deployment", ovn.Deployment, "Existing OVN NB client Deployment")
		fs.StringVar(&ovn.Container, "ovn-container", ovn.Container, "OVN NB client container")
		fs.StringVar(&ovn.Database, "ovn-db", ovn.Database, "OVN NB database endpoint as seen inside the container")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *planFile == "" && args[0] != "verify" {
		return fmt.Errorf("native %s requires --plan", args[0])
	}
	var p *nativePlan
	if *planFile != "" {
		p = &nativePlan{}
		if err := nativeReadJSON(*planFile, p); err != nil {
			return err
		}
		if err := validateNativePlan(*p); err != nil {
			return err
		}
	}
	c, err := a.client(opts)
	if err != nil {
		return err
	}
	if p != nil {
		if err := nativeCheckPlanCluster(ctx, c, *p, opts); err != nil {
			return err
		}
	}
	switch args[0] {
	case "upgrade":
		if p.Previous == nil {
			return fmt.Errorf("native upgrade requires a plan created with --upgrade-from")
		}
		if err := a.installNativePlan(ctx, c, *p, opts, *direct); err != nil {
			return err
		}
		return a.nativeVerify(ctx, c, p, p.Namespace, p.Deployment, p.Container, opts)
	case "verify":
		if p != nil {
			namespace, deployment, container = p.Namespace, p.Deployment, p.Container
		}
		return a.nativeVerify(ctx, c, p, namespace, deployment, container, opts)
	case "drain":
		if err := nativeCheckDrainState(ctx, c, *p); err != nil {
			return err
		}
		if err := a.nativeWaitDrained(ctx, c, opts, ovn); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, "Native cleanup verified: no destination-route intent, all retained ACKs match the current empty generation, and no UID-owned OVN routes or BFD rows remain. Keep materializers stopped.")
		return nil
	case "rollback", "uninstall":
		return a.restoreNative(ctx, c, *p, opts, ovn, *direct, args[0] == "uninstall")
	default:
		return fmt.Errorf("unsupported native lifecycle operation %q", args[0])
	}
}

func nativeCheckPlanCluster(ctx context.Context, c client.Client, p nativePlan, opts Options) error {
	uid, err := nativeClusterUID(ctx, c)
	if err != nil {
		return err
	}
	if uid != p.ClusterUID {
		return fmt.Errorf("wrong cluster: kube-system UID differs from the plan receipt")
	}
	if p.Context != "" && opts.Context != "" && p.Context != opts.Context {
		return fmt.Errorf("context differs from the plan receipt")
	}
	return nil
}

func nativeInstallIntentPreflight(ctx context.Context, c client.Client, p nativePlan) error {
	if p.Previous == nil {
		return nativeNoIntent(ctx, c)
	}
	return nativeAllACKs(ctx, c, false)
}

// Every retained acknowledgement must match current intent and generation.
// Vpcs never configured for this extension legitimately have no ACK at all.
func nativeAllACKs(ctx context.Context, c client.Client, requireEmpty bool) error {
	vpcs, err := nativeVPCList(ctx, c)
	if err != nil {
		return err
	}
	for _, vpc := range vpcs.Items {
		ack := nativeACK(vpc)
		if requireEmpty && ack.Routes != 0 {
			return fmt.Errorf("Vpc %s still has %d destination routes; delete tenant resources through their owning API and keep cleanup controllers running", ack.Name, ack.Routes)
		}
		if ack.State != "not-configured" && ack.State != "acknowledged" {
			return fmt.Errorf("Vpc %s has no current matching native ACK: %s", ack.Name, strings.Join(ack.Problems, "; "))
		}
	}
	return nil
}

func makeNativeUpgradePlan(ctx context.Context, c client.Client, opts Options, previous nativePlan, bundle NativeBundle) (nativePlan, error) {
	p := nativePlan{}
	if err := validateNativePlan(previous); err != nil {
		return p, err
	}
	if err := validateNativeBundle(bundle); err != nil {
		return p, err
	}
	if err := nativeCheckPlanCluster(ctx, c, previous, opts); err != nil {
		return p, err
	}
	if bundle.Target != previous.Bundle.Target || nativeImageReference(bundle.BaseImage) != nativeImageReference(previous.Bundle.BaseImage) {
		return p, fmt.Errorf("native upgrade requires the same pinned upstream target and base image; upgrade the full upstream Kube-OVN installation separately")
	}
	if bundle.Image == previous.Bundle.Image {
		return p, fmt.Errorf("target native image is already recorded in the previous receipt")
	}
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return p, err
	}
	var deployment appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: previous.Namespace, Name: previous.Deployment}, &deployment); err != nil {
		return p, err
	}
	i, err := validateNativeDeployment(&deployment, previous.Container)
	if err != nil {
		return p, err
	}
	expectedSpec, expectedTemplate := nativeExpectedState(previous)
	currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	if crd.GetUID() != previous.CRDUID || deployment.UID != previous.DeploymentUID || !nativeJSONEqual(currentSpec, expectedSpec) || !nativeJSONEqual(deployment.Spec.Template, expectedTemplate) {
		return p, fmt.Errorf("installed schema/template or resource identity differs from the previous receipt; reconcile the drift before upgrading")
	}
	ready, reason, err := nativeRolloutReady(ctx, c, &deployment, previous.Container, previous.Bundle.Image)
	if err != nil {
		return p, err
	}
	if !ready {
		return p, fmt.Errorf("previous native rollout is incomplete: %s", reason)
	}
	if err := nativeAllACKs(ctx, c, false); err != nil {
		return p, err
	}
	p = nativePlan{APIVersion: nativeAPIVersion, Kind: "NativeInstallPlan", Bundle: bundle, Context: opts.Context, ClusterUID: previous.ClusterUID, Namespace: previous.Namespace, Deployment: previous.Deployment, Container: previous.Container, CRDUID: previous.CRDUID, CRDVersion: crd.GetResourceVersion(), CRDSpec: currentSpec, DeploymentUID: previous.DeploymentUID, DeploymentVersion: deployment.ResourceVersion, Template: *deployment.Spec.Template.DeepCopy(), Owners: append(nativeManagers(crd), nativeManagers(&deployment)...), OriginalRuntimeImages: []string{previous.Bundle.Image}, Previous: &previous}
	p.CRDPatch, p.DeploymentPatch = nativePlanPatches(p, i)
	return p, validateNativePlan(p)
}

func (a *App) nativeVerify(ctx context.Context, c client.Client, p *nativePlan, namespace, deployment, container string, opts Options) error {
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	props, err := nativeCRDProperties(crd)
	if err != nil {
		return err
	}
	spec, _, _ := unstructured.NestedMap(props, "spec", "properties", "destinationRoutes")
	status, _, _ := unstructured.NestedMap(props, "status", "properties", "destinationRoutes")
	if !nativeJSONEqual(NativeSchema{Spec: spec, Status: status}, nativeSchema()) {
		return fmt.Errorf("native destination-routes.v1 schema is absent or incompatible")
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment}, &d); err != nil {
		return err
	}
	i, err := validateNativeDeployment(&d, container)
	if err != nil {
		return err
	}
	if p != nil {
		if err := nativeCheckPlanCluster(ctx, c, *p, opts); err != nil {
			return err
		}
		expectedSpec, expectedTemplate := nativeExpectedState(*p)
		currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
		if crd.GetUID() != p.CRDUID || d.UID != p.DeploymentUID || !nativeJSONEqual(currentSpec, expectedSpec) || !nativeJSONEqual(d.Spec.Template, expectedTemplate) {
			return fmt.Errorf("native installation differs from the exact plan receipt")
		}
	}
	ready, reason, err := nativeRolloutReady(ctx, c, &d, container, d.Spec.Template.Spec.Containers[i].Image)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("native rollout is not ready: %s", reason)
	}
	if err := nativeAllACKs(ctx, c, false); err != nil {
		return err
	}
	if err := a.nativeStatus(ctx, c, namespace, deployment, container, opts); err != nil {
		return err
	}
	if opts.Output != "json" && opts.Output != "yaml" {
		fmt.Fprintln(a.Out, "Verified native schema, controller rollout, and current ACKs. No tenant packet test was performed.")
	}
	return nil
}

func nativeCheckDrainState(ctx context.Context, c client.Client, p nativePlan) error {
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	var deployment appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Deployment}, &deployment); err != nil {
		return err
	}
	expectedSpec, expectedTemplate := nativeExpectedState(p)
	currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	if crd.GetUID() != p.CRDUID || deployment.UID != p.DeploymentUID || !nativeJSONEqual(currentSpec, expectedSpec) || !nativeJSONEqual(deployment.Spec.Template, expectedTemplate) {
		return fmt.Errorf("native cleanup requires the exact installed plan state; use rollback to recover a partial install")
	}
	return nil
}

func (a *App) nativeWaitDrained(ctx context.Context, c client.Client, opts Options, ovn nativeOVNOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last error
	for {
		if err := waitCtx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("native cleanup is not complete: %w: %v", last, err)
			}
			return err
		}
		err := nativeAllACKs(waitCtx, c, true)
		if err == nil {
			err = a.nativeOVNEmpty(waitCtx, c, opts, ovn)
		}
		if err == nil {
			return nil
		}
		if waitCtx.Err() != nil && last != nil {
			return fmt.Errorf("native cleanup is not complete: %w: %v", last, waitCtx.Err())
		}
		last = err
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("native cleanup is not complete: %w: %v", err, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// nativeOVNEmpty examines all rows, including orphan BFD rows, rather than
// trusting Kubernetes ACKs or route attachment to a router.
func (a *App) nativeOVNEmpty(ctx context.Context, c client.Client, opts Options, ovn nativeOVNOptions) error {
	if ovn.Namespace == "" || ovn.Deployment == "" || ovn.Container == "" || ovn.Database == "" {
		return fmt.Errorf("OVN namespace, Deployment, container, and database must be nonempty")
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ovn.Namespace, Name: ovn.Deployment}, &d); err != nil {
		return fmt.Errorf("read OVN client Deployment: %w", err)
	}
	i, err := nativeContainerIndex(&d, ovn.Container)
	if err != nil {
		return err
	}
	ready, reason, pods, err := nativeRolloutSnapshot(ctx, c, &d, ovn.Container, d.Spec.Template.Spec.Containers[i].Image)
	if err != nil {
		return err
	}
	if !ready || len(pods) == 0 {
		return fmt.Errorf("OVN client Deployment is not fully ready: %s", reason)
	}
	// Read each ready replica. A divergent or unhealthy NB replica must not be
	// accepted as evidence that the shared OVN database is empty.
	for _, pod := range pods {
		for _, table := range []string{"Logical_Router_Static_Route", "BFD"} {
			var stdout, stderr bytes.Buffer
			command := []string{"ovn-nbctl", "--timeout=15", "--db=" + ovn.Database, "--format=json", "--columns=_uuid,external_ids", "list", table}
			if err := a.execPod(ctx, opts, ovn.Namespace, pod.Name, ovn.Container, command, &stdout, &stderr); err != nil {
				return fmt.Errorf("read OVN %s via %s/%s: %w", table, ovn.Namespace, pod.Name, err)
			}
			if err := nativeCheckOVNRows(stdout.Bytes(), table); err != nil {
				return err
			}
		}
	}
	return nil
}

func nativeCheckOVNRows(data []byte, table string) error {
	var result struct {
		Headings []string            `json:"headings"`
		Data     [][]json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("invalid OVN %s JSON: %w", table, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("unexpected trailing OVN %s output", table)
	}
	index := -1
	for i, h := range result.Headings {
		if h == "external_ids" {
			if index >= 0 {
				return fmt.Errorf("OVN %s contains duplicate external_ids headings", table)
			}
			index = i
		}
	}
	if index < 0 || result.Data == nil {
		return fmt.Errorf("OVN %s output is missing external_ids or data", table)
	}
	for _, row := range result.Data {
		if len(row) != len(result.Headings) {
			return fmt.Errorf("OVN %s row has unexpected columns", table)
		}
		var encoded []json.RawMessage
		if err := json.Unmarshal(row[index], &encoded); err != nil || len(encoded) != 2 || string(encoded[0]) != `"map"` {
			return fmt.Errorf("OVN %s external_ids is not a map", table)
		}
		var entries [][]string
		if err := json.Unmarshal(encoded[1], &entries); err != nil {
			return fmt.Errorf("invalid OVN %s external_ids entries", table)
		}
		for _, entry := range entries {
			if len(entry) != 2 {
				return fmt.Errorf("invalid OVN %s external_ids pair", table)
			}
			if entry[0] == "kube-ovn.io/destination-vpc-uid" {
				return fmt.Errorf("OVN %s still contains a destination-vpc-uid owned row; leave the patched controller running until cleanup completes", table)
			}
		}
	}
	return nil
}

func nativeRootPlan(p nativePlan) nativePlan {
	for p.Previous != nil {
		p = *p.Previous
	}
	return p
}

func nativeRestoreTarget(p nativePlan, uninstall bool) (map[string]any, corev1.PodTemplateSpec, string, bool, error) {
	target := p
	stock := p.Previous == nil || uninstall
	if uninstall {
		target = nativeRootPlan(p)
	}
	if len(target.OriginalRuntimeImages) != 1 || !digestImage.MatchString(target.OriginalRuntimeImages[0]) {
		return nil, corev1.PodTemplateSpec{}, "", stock, fmt.Errorf("receipt does not contain exactly one verified original immutable image; restoration cannot guess a mutable tag")
	}
	image := target.OriginalRuntimeImages[0]
	if stock && target.Bundle.BaseImage != "" && nativeImageReference(image) != nativeImageReference(target.Bundle.BaseImage) {
		return nil, corev1.PodTemplateSpec{}, "", stock, fmt.Errorf("receipt original image differs from the verified native baseline")
	}
	template := *target.Template.DeepCopy()
	for i := range template.Spec.Containers {
		if template.Spec.Containers[i].Name == target.Container {
			template.Spec.Containers[i].Image = image
		}
	}
	return target.CRDSpec, template, image, stock, nil
}

// restoreNative reverses only the managed image and, when returning to stock,
// the two schema properties. Each stage is idempotent for the exact receipt.
func (a *App) restoreNative(ctx context.Context, c client.Client, p nativePlan, opts Options, ovn nativeOVNOptions, direct, uninstall bool) error {
	if err := validateNativePlan(p); err != nil {
		return err
	}
	if err := nativeCheckPlanCluster(ctx, c, p, opts); err != nil {
		return err
	}
	targetSpec, targetTemplate, image, stock, err := nativeRestoreTarget(p, uninstall)
	if err != nil {
		return err
	}
	expectedSpec, expectedTemplate := nativeExpectedState(p)
	crd := nativeCRD()
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	var deployment appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Deployment}, &deployment); err != nil {
		return err
	}
	if crd.GetUID() != p.CRDUID || deployment.UID != p.DeploymentUID {
		return fmt.Errorf("receipt is stale: native CRD or Deployment UID changed")
	}
	i, err := validateNativeDeployment(&deployment, p.Container)
	if err != nil {
		return err
	}
	currentSpec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	imageRestored := nativeJSONEqual(deployment.Spec.Template, targetTemplate)
	schemaRestored := nativeJSONEqual(currentSpec, targetSpec)
	if !nativeJSONEqual(currentSpec, expectedSpec) && !schemaRestored {
		return fmt.Errorf("native CRD drifted from both the installed and restoration schema; no changes applied")
	}
	if !imageRestored && !nativeJSONEqual(deployment.Spec.Template, expectedTemplate) && !(p.Previous == nil && nativeJSONEqual(deployment.Spec.Template, p.Template)) {
		return fmt.Errorf("native Deployment template drifted from both the installed and restoration state; no changes applied")
	}
	if stock && schemaRestored && !imageRestored && !nativeJSONEqual(deployment.Spec.Template, p.Template) {
		return fmt.Errorf("native schema is already absent while the patched image remains; inspect partial state before restoring")
	}
	owners := append(nativeManagers(crd), nativeManagers(&deployment)...)
	if len(owners) > 0 && !direct {
		return fmt.Errorf("owner-managed resources detected (%s): coordinated restoration requires --direct-ownership and matching owner desired state", strings.Join(owners, ", "))
	}
	if stock {
		fmt.Fprintln(a.Out, "Waiting for empty destination intent, current empty ACKs, and independent absence of UID-owned OVN routes/BFD before restoring stock.")
		if err := a.nativeWaitDrained(ctx, c, opts, ovn); err != nil {
			return err
		}
	} else if err := nativeAllACKs(ctx, c, false); err != nil {
		return fmt.Errorf("compatible rollback preflight: %w", err)
	}
	imagePatch := []nativePatchOperation{{"test", "/metadata/uid", deployment.UID}, {"test", "/metadata/resourceVersion", deployment.ResourceVersion}, {"test", "/spec/template", deployment.Spec.Template}, {"replace", fmt.Sprintf("/spec/template/spec/containers/%d/image", i), image}}
	imageData, _ := json.Marshal(imagePatch)
	schemaPatch := nativeSchemaRestorePatch(crd, p.Bundle.Schema)
	schemaData, _ := json.Marshal(schemaPatch)
	if !imageRestored {
		object := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: p.Deployment, Namespace: p.Namespace}}
		if err := c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, imageData), client.DryRunAll); err != nil {
			return fmt.Errorf("restoration image dry-run failed; no changes applied: %w", err)
		}
		if !nativeJSONEqual(object.Spec.Template, targetTemplate) {
			return fmt.Errorf("restoration image admission changed fields beyond the image; no changes applied")
		}
	}
	if stock && !schemaRestored {
		if err := c.Patch(ctx, nativeCRD(), client.RawPatch(types.JSONPatchType, schemaData), client.DryRunAll); err != nil {
			return fmt.Errorf("restoration schema dry-run failed; no changes applied: %w", err)
		}
	}
	if !imageRestored {
		if stock {
			if err := nativeAllACKs(ctx, c, true); err != nil {
				return err
			}
			if err := a.nativeOVNEmpty(ctx, c, opts, ovn); err != nil {
				return err
			}
		} else if err := nativeAllACKs(ctx, c, false); err != nil {
			return err
		}
		object := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: p.Deployment, Namespace: p.Namespace}}
		if err := c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, imageData)); err != nil {
			return fmt.Errorf("restoration image patch failed; schema retained: %w", err)
		}
		if !nativeJSONEqual(object.Spec.Template, targetTemplate) {
			return fmt.Errorf("PARTIAL RESTORATION: admission changed the Deployment beyond the image; schema retained, inspect before retrying")
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := nativeWaitRollout(waitCtx, c, p.Namespace, p.Deployment, p.Container, p.DeploymentUID, image, targetTemplate); err != nil {
		return fmt.Errorf("PARTIAL RESTORATION: original image applied but rollout unverified; schema retained; retry with the same receipt: %w", err)
	}
	// Fetch fresh RV after rollout while accepting only the exact schema snapshot.
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	currentSpec, _, _ = unstructured.NestedMap(crd.Object, "spec")
	if crd.GetUID() != p.CRDUID || (!nativeJSONEqual(currentSpec, expectedSpec) && !nativeJSONEqual(currentSpec, targetSpec)) {
		return fmt.Errorf("PARTIAL RESTORATION: CRD drifted while restoring the controller; no schema change applied")
	}
	if stock && !nativeJSONEqual(currentSpec, targetSpec) {
		if len(nativeManagers(crd)) > 0 && !direct {
			return fmt.Errorf("PARTIAL RESTORATION: CRD acquired an external manager; coordinate it before retrying")
		}
		if err := nativeAllACKs(ctx, c, true); err != nil {
			return fmt.Errorf("PARTIAL RESTORATION: intent changed after stock rollout; schema retained: %w", err)
		}
		if err := a.nativeOVNEmpty(ctx, c, opts, ovn); err != nil {
			return fmt.Errorf("PARTIAL RESTORATION: OVN cleanup no longer verified; schema retained: %w", err)
		}
		schemaData, _ = json.Marshal(nativeSchemaRestorePatch(crd, p.Bundle.Schema))
		if err := c.Patch(ctx, nativeCRD(), client.RawPatch(types.JSONPatchType, schemaData)); err != nil {
			return fmt.Errorf("PARTIAL RESTORATION: original image is ready but schema removal failed; retry with the same receipt: %w", err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return err
	}
	currentSpec, _, _ = unstructured.NestedMap(crd.Object, "spec")
	if crd.GetUID() != p.CRDUID || !nativeJSONEqual(currentSpec, targetSpec) {
		return fmt.Errorf("restoration schema verification failed")
	}
	if !stock {
		if err := nativeAllACKs(ctx, c, false); err != nil {
			return fmt.Errorf("previous native image restored but ACK verification failed: %w", err)
		}
		fmt.Fprintln(a.Out, "Previous native image restored; compatible schema retained and current ACKs verified. The previous embedded receipt is the active installation state.")
	} else {
		fmt.Fprintln(a.Out, "Stock native image restored and ready; only the two extension schema properties were removed. Tenant resources, receipts, and upstream Kube-OVN were not deleted. Keep extension materializers disabled.")
	}
	fmt.Fprintln(a.Out, "No baseline or tenant packet test was performed.")
	return nil
}

func nativeSchemaRestorePatch(crd *unstructured.Unstructured, schema NativeSchema) []nativePatchOperation {
	patch := []nativePatchOperation{{"test", "/metadata/uid", crd.GetUID()}, {"test", "/metadata/resourceVersion", crd.GetResourceVersion()}}
	for _, section := range []string{"spec", "status"} {
		fragment := schema.Spec
		if section == "status" {
			fragment = schema.Status
		}
		field := "/spec/versions/0/schema/openAPIV3Schema/properties/" + section + "/properties/destinationRoutes"
		patch = append(patch, nativePatchOperation{"test", field, fragment}, nativePatchOperation{Op: "remove", Path: field})
	}
	return patch
}
