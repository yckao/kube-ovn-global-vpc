package vpcctl

import (
	"context"
	"fmt"
	"io"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func (a *App) restConfig(opts Options) (*rest.Config, error) {
	cfg, err := kubeAccess(opts).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot load Kubernetes access; check --kubeconfig and --context")
	}
	cfg.Timeout = 15 * time.Second
	cfg.UserAgent = "vpcctl"
	return cfg, nil
}

// execPod uses the Kubernetes exec subresource; no kubectl or local shell is used.
func (a *App) execPod(ctx context.Context, opts Options, namespace, pod, container string, argv []string, stdout, stderr io.Writer) error {
	if a.PodExecutor != nil {
		return a.PodExecutor(ctx, opts, namespace, pod, container, argv, stdout, stderr)
	}
	cfg, err := a.restConfig(opts)
	if err != nil {
		return err
	}
	cfg.Timeout = 0 // The caller's context bounds the streaming connection.
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	request := c.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: container, Command: argv, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("create Kubernetes exec transport: %w", err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}

func (a *App) requestToken(ctx context.Context, opts Options, namespace, account string, duration time.Duration) (string, time.Time, error) {
	if a.TokenRequester != nil {
		return a.TokenRequester(ctx, opts, namespace, account, duration)
	}
	cfg, err := a.restConfig(opts)
	if err != nil {
		return "", time.Time{}, err
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", time.Time{}, err
	}
	seconds := int64(duration.Seconds())
	token, err := c.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, account,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request location-scoped access token: %w", err)
	}
	if token.Status.Token == "" || !token.Status.ExpirationTimestamp.After(time.Now()) {
		return "", time.Time{}, fmt.Errorf("token issuer returned empty or expired access")
	}
	return token.Status.Token, token.Status.ExpirationTimestamp.Time, nil
}
