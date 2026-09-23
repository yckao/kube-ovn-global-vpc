package vpcctl

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const accessFixtureToken = "issued-evaluation-token-fixture"

func accessFixture(t *testing.T, mutate func(*clientcmdapi.Config)) (string, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "access test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	raw := clientcmdapi.NewConfig()
	raw.CurrentContext = "other"
	raw.Clusters["other"] = &clientcmdapi.Cluster{Server: "http://wrong.example.invalid", InsecureSkipTLSVerify: true}
	raw.Contexts["other"] = &clientcmdapi.Context{Cluster: "other", AuthInfo: "administrator", Namespace: "wrong-namespace"}
	raw.Clusters["selected"] = &clientcmdapi.Cluster{Server: "https://authority.example.invalid", CertificateAuthorityData: ca,
		TLSServerName: "certificate.example.invalid", ProxyURL: "http://proxy-user:proxy-secret@proxy.example.invalid", DisableCompression: true}
	raw.Contexts["chosen"] = &clientcmdapi.Context{Cluster: "selected", AuthInfo: "administrator", Namespace: "admin-namespace"}
	raw.AuthInfos["administrator"] = &clientcmdapi.AuthInfo{Token: "administrator-token-fixture",
		TokenFile: "/does/not/exist/admin-token", Username: "admin", Password: "administrator-password-fixture",
		ClientKeyData: []byte("administrator-private-key-fixture"), ClientCertificate: "/does/not/exist/admin-certificate",
		Exec:        &clientcmdapi.ExecConfig{Command: "/does/not/exist/auth-helper", APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode},
		Impersonate: "administrator-impersonation-fixture"}
	if mutate != nil {
		mutate(raw)
	}
	config, err := clientcmd.Write(*raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.kubeconfig")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	return path, ca
}

func accessArguments(source, output string) []string {
	return []string{"--kubeconfig", source, "--context", "chosen", "access", "issue", "--binding-namespace", "location-a", "--output", output}
}

func TestAccessIssueCopiesOnlySelectedTrustAndIssuedToken(t *testing.T) {
	source, ca := accessFixture(t, nil)
	original, _ := os.ReadFile(source)
	output := filepath.Join(t.TempDir(), "location.kubeconfig")
	expiry := time.Now().Add(42 * time.Minute).Truncate(time.Second)
	var stdout, stderr bytes.Buffer
	calls := 0
	a := App{Out: &stdout, ErrOut: &stderr, TokenRequester: func(ctx context.Context, opts Options, namespace, account string, duration time.Duration) (string, time.Time, error) {
		calls++
		if opts.Kubeconfig != source || opts.Context != "chosen" || namespace != "location-a" || account != "location-reader" || duration != 2*time.Hour {
			t.Fatal("TokenRequest lost the selected context, account or duration")
		}
		if _, present := ctx.Deadline(); !present {
			t.Fatal("TokenRequest has no timeout")
		}
		return accessFixtureToken, expiry, nil
	}}
	args := append(accessArguments(source, output), "--service-account", "location-reader", "--duration", "2h")
	if err := a.Run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("expected exactly one TokenRequest")
	}
	stat, err := os.Stat(output)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("issued file is not private")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal("issued kubeconfig is invalid")
	}
	if len(issued.Clusters) != 1 || len(issued.Contexts) != 1 || len(issued.AuthInfos) != 1 || issued.CurrentContext != "authority" {
		t.Fatal("issued kubeconfig contains unrelated source configuration")
	}
	cluster := issued.Clusters["authority"]
	if cluster.Server != "https://authority.example.invalid" || !bytes.Equal(cluster.CertificateAuthorityData, ca) || cluster.TLSServerName != "certificate.example.invalid" || cluster.CertificateAuthority != "" || cluster.ProxyURL != "" || cluster.InsecureSkipTLSVerify || cluster.DisableCompression {
		t.Fatal("issued cluster did not preserve only selected trust settings")
	}
	if issued.Contexts["authority"].Namespace != "location-a" || issued.AuthInfos["location-reader"].Token != accessFixtureToken {
		t.Fatal("issued identity or binding namespace is incorrect")
	}
	for _, forbidden := range []string{"administrator-token-fixture", "administrator-password-fixture", "administrator-private-key-fixture", "administrator-impersonation-fixture", "admin-token", "admin-certificate", "auth-helper", "proxy-secret", "wrong.example.invalid"} {
		if bytes.Contains(data, []byte(forbidden)) || strings.Contains(stdout.String()+stderr.String(), forbidden) {
			t.Fatal("administrator configuration leaked into access artifact or logs")
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), accessFixtureToken) || !strings.Contains(stdout.String(), expiry.UTC().Format(time.RFC3339)) {
		t.Fatal("output leaked the token or omitted the actual expiry")
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(original, after) {
		t.Fatal("issuing access modified the administrator kubeconfig")
	}
}

func TestAccessIssueFlattensSelectedRelativeCAFile(t *testing.T) {
	source, ca := accessFixture(t, func(raw *clientcmdapi.Config) {
		cluster := raw.Clusters["selected"]
		cluster.CertificateAuthorityData = nil
		cluster.CertificateAuthority = "trusted-ca.pem"
	})
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), "trusted-ca.pem"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	cluster, err := accessCluster(Options{Kubeconfig: source, Context: "chosen"})
	if err != nil || !bytes.Equal(cluster.CertificateAuthorityData, ca) || cluster.CertificateAuthority != "" {
		t.Fatal("selected CA file was not flattened into standalone trust data")
	}
}

func TestAccessIssueRejectsUnsafeTrustBeforeTokenRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*clientcmdapi.Cluster)
	}{
		{"http", func(c *clientcmdapi.Cluster) { c.Server = "http://authority.example.invalid" }},
		{"embedded-user", func(c *clientcmdapi.Cluster) { c.Server = "https://admin:secret@authority.example.invalid" }},
		{"query-credential", func(c *clientcmdapi.Cluster) { c.Server += "?token=secret" }},
		{"insecure-tls", func(c *clientcmdapi.Cluster) { c.InsecureSkipTLSVerify = true }},
		{"missing-ca", func(c *clientcmdapi.Cluster) { c.CertificateAuthorityData = nil }},
		{"invalid-ca", func(c *clientcmdapi.Cluster) { c.CertificateAuthorityData = []byte("invalid") }},
		{"private-key-in-ca", func(c *clientcmdapi.Cluster) {
			c.CertificateAuthorityData = append(c.CertificateAuthorityData, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")})...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, _ := accessFixture(t, func(raw *clientcmdapi.Config) { tc.mutate(raw.Clusters["selected"]) })
			output := filepath.Join(t.TempDir(), "access")
			a := App{TokenRequester: func(context.Context, Options, string, string, time.Duration) (string, time.Time, error) {
				t.Fatal("unsafe source trust triggered TokenRequest")
				return "", time.Time{}, nil
			}}
			if err := a.Run(context.Background(), accessArguments(source, output)); err == nil {
				t.Fatal("unsafe source trust was accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatal("invalid source left an output artifact")
			}
		})
	}
}

func TestAccessIssueRefusesExistingFilesAndSymlinksBeforeIssuance(t *testing.T) {
	source, _ := accessFixture(t, nil)
	for _, symlink := range []bool{false, true} {
		root := t.TempDir()
		original := filepath.Join(root, "original")
		if err := os.WriteFile(original, []byte("existing content"), 0644); err != nil {
			t.Fatal(err)
		}
		output := original
		if symlink {
			output = filepath.Join(root, "output-link")
			if err := os.Symlink(original, output); err != nil {
				t.Fatal(err)
			}
		}
		a := App{TokenRequester: func(context.Context, Options, string, string, time.Duration) (string, time.Time, error) {
			t.Fatal("existing destination triggered a TokenRequest")
			return "", time.Time{}, nil
		}}
		if err := a.Run(context.Background(), accessArguments(source, output)); err == nil {
			t.Fatal("existing destination was accepted")
		}
		data, _ := os.ReadFile(original)
		stat, _ := os.Stat(original)
		if string(data) != "existing content" || stat.Mode().Perm() != 0644 {
			t.Fatal("existing credential file was changed")
		}
	}
}

func TestAccessIssueFailureCleansOutputAndSanitizesErrors(t *testing.T) {
	source, _ := accessFixture(t, nil)
	for _, tc := range []struct {
		name  string
		token string
		time  time.Time
		err   error
	}{
		{"issuer-error", "", time.Time{}, errors.New("issuer echoed " + accessFixtureToken)},
		{"empty-token", "", time.Now().Add(time.Hour), nil},
		{"expired-token", accessFixtureToken, time.Now().Add(-time.Hour), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "access")
			var logs bytes.Buffer
			a := App{Out: &logs, ErrOut: &logs, TokenRequester: func(context.Context, Options, string, string, time.Duration) (string, time.Time, error) {
				return tc.token, tc.time, tc.err
			}}
			err := a.Run(context.Background(), accessArguments(source, output))
			if err == nil || strings.Contains(err.Error()+logs.String(), accessFixtureToken) {
				t.Fatal("failed issuance succeeded or exposed credential material")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatal("failed issuance left an output artifact")
			}
		})
	}
}

func TestAccessIssueDefaultsAndMachineReportContainNoToken(t *testing.T) {
	source, _ := accessFixture(t, nil)
	var report bytes.Buffer
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	a := App{Out: &report, TokenRequester: func(_ context.Context, _ Options, namespace, account string, duration time.Duration) (string, time.Time, error) {
		if namespace != "location-a" || account != "binding-reporter" || duration != time.Hour {
			t.Fatal("unexpected default issuance scope or duration")
		}
		return accessFixtureToken, expiry, nil
	}}
	args := append([]string{"-o", "json"}, accessArguments(source, filepath.Join(t.TempDir(), "access"))...)
	if err := a.Run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(report.Bytes(), &result); err != nil || result["expiresAt"] != expiry.UTC().Format(time.RFC3339) || len(result) != 4 || strings.Contains(report.String(), accessFixtureToken) {
		t.Fatal("machine report leaked access or omitted expiry")
	}
}

type accessFailingWriter struct{}

func (accessFailingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestAccessIssueOutputAndCancellationFailuresRemoveFile(t *testing.T) {
	source, _ := accessFixture(t, nil)
	output := filepath.Join(t.TempDir(), "access")
	a := App{Out: accessFailingWriter{}, TokenRequester: func(context.Context, Options, string, string, time.Duration) (string, time.Time, error) {
		return accessFixtureToken, time.Now().Add(time.Hour), nil
	}}
	if err := a.Run(context.Background(), accessArguments(source, output)); err == nil {
		t.Fatal("report failure was ignored")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("report failure left an output artifact")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.TokenRequester = func(context.Context, Options, string, string, time.Duration) (string, time.Time, error) {
		t.Fatal("canceled issuance requested a token")
		return "", time.Time{}, nil
	}
	if err := a.Run(ctx, accessArguments(source, output)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not preserved")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("canceled issuance left an output artifact")
	}
}

func TestAccessIssueInvalidFlagsDoNotRequestToken(t *testing.T) {
	for _, extra := range [][]string{{"--binding-namespace", "Invalid"}, {"--service-account", "Invalid"}, {"--duration", "0s"}, {"--duration", "500ms"}, {"unexpected"}} {
		a := App{TokenRequester: func(context.Context, Options, string, string, time.Duration) (string, time.Time, error) {
			t.Fatal("invalid arguments requested a token")
			return "", time.Time{}, nil
		}}
		args := append(accessArguments("/does/not/exist", filepath.Join(t.TempDir(), "output")), extra...)
		if err := a.Run(context.Background(), args); err == nil {
			t.Fatal("invalid arguments were accepted")
		}
	}
}
