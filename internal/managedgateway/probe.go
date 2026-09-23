package managedgateway

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// The adapter runs against the installed managed module, including old gateway
// generations. It only filters a memory copy; no runtime file or network state
// is changed. Embedding it avoids requiring a runtime upgrade before rollout.
//
//go:embed rollout_probe.py
var rolloutProbeScript string

const maxProbeConfigBytes = 1 << 20

func probeConfig(desired map[string]any) ([]byte, error) {
	if desired == nil {
		return nil, fmt.Errorf("desired rollout configuration is missing")
	}
	data, err := json.Marshal(desired)
	if err != nil || len(data) > maxProbeConfigBytes {
		return nil, fmt.Errorf("invalid desired rollout configuration")
	}
	return data, nil
}

// NativeProbe performs a bounded read-only native BFD/BGP/FIB check inside an
// existing owned gateway for retained desired traffic. Newly added delegations
// are checked once the replacement becomes a survivor, not on an old generation
// that cannot yet know them. Pod Ready alone is not remote convergence.
func NativeProbe(config *rest.Config) func(context.Context, *core.Pod, map[string]any) (bool, error) {
	return func(ctx context.Context, pod *core.Pod, desired map[string]any) (bool, error) {
		if config == nil {
			return false, fmt.Errorf("native rollout probe is not configured")
		}
		data, err := probeConfig(desired)
		if err != nil {
			return false, err
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return false, err
		}
		request := client.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec").VersionedParams(&core.PodExecOptions{Container: "gateway", Command: []string{"python3", "-c", rolloutProbeScript}, Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
		exec, err := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		// Gateway configuration and command output never enter a status/error.
		if err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: bytes.NewReader(data), Stdout: io.Discard, Stderr: io.Discard}); err != nil {
			return false, nil
		}
		return true, nil
	}
}
