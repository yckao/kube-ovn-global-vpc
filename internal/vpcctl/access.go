package vpcctl

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

const accessUsage = `Usage: vpcctl [global flags] access issue --binding-namespace NAMESPACE --output NEWFILE [flags]

Issue short-lived evaluation access for an existing location service account.
The management kubeconfig must be permitted to create its TokenRequest.
The service account and its location-scoped RBAC must already exist.

Only the selected cluster's HTTPS endpoint, trusted CA and optional TLS server
name are copied. Administrator credentials and proxy settings are never copied.
The issued kubeconfig is a new private file (0600); existing files are refused.
The command reports the API server's actual expiry and never prints the token.

For renewal, issue to another new private file and update the site's access
Secret. This command is not a credential-renewal daemon.
`

func (a *App) runAccess(ctx context.Context, args []string, opts Options) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprint(a.Out, accessUsage)
		return err
	}
	if args[0] != "issue" {
		return fmt.Errorf("unknown access command; use access issue --help")
	}
	var namespace, account, output string
	var duration time.Duration
	flags := pflag.NewFlagSet("vpcctl access issue", pflag.ContinueOnError)
	flags.SetOutput(a.ErrOut)
	flags.StringVar(&namespace, "binding-namespace", "", "Existing location binding namespace (required)")
	flags.StringVar(&account, "service-account", "binding-reporter", "Existing location-scoped service account")
	flags.StringVar(&output, "output", "", "New private kubeconfig file; never overwrite an existing file (required)")
	flags.DurationVar(&duration, "duration", time.Hour, "Requested token lifetime; the API server determines the actual expiry")
	if hasHelp(args[1:]) {
		fmt.Fprint(a.Out, accessUsage)
		_, err := fmt.Fprint(a.Out, "\nFlags:\n"+flags.FlagUsages())
		return err
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(account)) != 0 || output == "" {
		return fmt.Errorf("provide a valid --binding-namespace, --service-account and --output NEWFILE without positional arguments")
	}
	if duration < time.Second || duration%time.Second != 0 {
		return fmt.Errorf("--duration must be a positive whole number of seconds")
	}
	cluster, err := accessCluster(opts)
	if err != nil {
		return err
	}
	// Reserve the destination before issuing a credential. O_EXCL also refuses
	// existing symlinks, preventing accidental replacement of another file.
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot create output: choose a new file in an existing writable private directory")
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(output)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("cannot protect the private output file")
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	token, expiry, err := a.requestToken(ctx, opts, namespace, account, duration)
	if err != nil {
		// API/authentication errors may contain server responses or plugin output.
		// Never include those details in credential-issuing command output.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("could not issue location access; verify the selected context, service account and TokenRequest RBAC")
	}
	if token == "" || !expiry.After(time.Now()) {
		return fmt.Errorf("the API server returned empty or expired location access")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "authority"
	config.Clusters["authority"] = cluster
	config.Contexts["authority"] = &clientcmdapi.Context{Cluster: "authority", AuthInfo: account, Namespace: namespace}
	config.AuthInfos[account] = &clientcmdapi.AuthInfo{Token: token}
	data, err := clientcmd.Write(*config)
	if err != nil {
		return fmt.Errorf("cannot encode private location access")
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("cannot write private location access")
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("cannot persist private location access")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("cannot close private location access")
	}
	if err := a.accessReport(opts, namespace, account, expiry); err != nil {
		return fmt.Errorf("cannot report location access issuance; private output removed")
	}
	complete = true
	return nil
}

// accessCluster constructs an allowlisted cluster record without loading any
// administrator key files or executing any kubeconfig authentication plugin.
func accessCluster(opts Options) (*clientcmdapi.Cluster, error) {
	raw, err := kubeAccess(opts).RawConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot load Kubernetes access; check --kubeconfig and --context")
	}
	selected := opts.Context
	if selected == "" {
		selected = raw.CurrentContext
	}
	context := raw.Contexts[selected]
	if context == nil || raw.Clusters[context.Cluster] == nil {
		return nil, fmt.Errorf("selected kubeconfig context has no cluster")
	}
	source := raw.Clusters[context.Cluster]
	endpoint, err := url.Parse(source.Server)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || source.InsecureSkipTLSVerify {
		return nil, fmt.Errorf("location access requires an HTTPS endpoint without embedded credentials and with TLS verification enabled")
	}
	ca := source.CertificateAuthorityData
	if len(ca) == 0 && source.CertificateAuthority != "" {
		path := source.CertificateAuthority
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(source.LocationOfOrigin), path)
		}
		ca, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read the selected cluster's trusted CA file")
		}
	}
	ca, err = accessCA(ca)
	if err != nil {
		return nil, err
	}
	return &clientcmdapi.Cluster{Server: source.Server, CertificateAuthorityData: ca, TLSServerName: source.TLSServerName}, nil
}

func accessCA(data []byte) ([]byte, error) {
	var certificates bytes.Buffer
	for len(bytes.TrimSpace(data)) != 0 {
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, fmt.Errorf("selected cluster requires an explicit PEM certificate-authority containing only certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("selected cluster has an invalid certificate-authority certificate")
		}
		_ = pem.Encode(&certificates, block)
		data = rest
	}
	if certificates.Len() == 0 {
		return nil, fmt.Errorf("selected cluster requires an explicit trusted certificate-authority")
	}
	return certificates.Bytes(), nil
}

func (a *App) accessReport(opts Options, namespace, account string, expiry time.Time) error {
	expires := expiry.UTC().Format(time.RFC3339)
	if opts.Output == "json" || opts.Output == "yaml" {
		data, err := json.MarshalIndent(map[string]string{"bindingNamespace": namespace, "serviceAccount": account,
			"expiresAt": expires, "purpose": "short-lived evaluation access"}, "", "  ")
		if err != nil {
			return err
		}
		if opts.Output == "yaml" {
			data, err = yaml.JSONToYAML(data)
			if err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(a.Out, string(data))
		return err
	}
	_, err := fmt.Fprintf(a.Out, "Wrote private location access for %s/%s; expires %s.\nShort-lived evaluation access; issue a new file and update the site's access Secret before expiry.\n", namespace, account, expires)
	return err
}
