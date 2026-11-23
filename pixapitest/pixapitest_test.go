package pixapitest_test

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
	"github.com/victorcondackdev/pix-cobranca-go/pixapitest"
)

// TestRunPlainHTTP runs the suite against the mock without TLS, with a
// custom key and some groups skipped. The mTLS run lives in package
// mockpsp.
func TestRunPlainHTTP(t *testing.T) {
	const key = "+5521987654321"
	psp, err := mockpsp.New(mockpsp.Config{Keys: []string{key}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(psp.Handler())
	defer ts.Close()
	defer psp.Close()
	c, err := pixapi.NewClient(ts.URL+psp.BasePath(), pixapi.WithOAuth2(ts.URL+"/oauth/token", "dev-client", "dev-secret"))
	if err != nil {
		t.Fatal(err)
	}
	pixapitest.Run(t, c.BaseURL(), c,
		pixapitest.WithChave(key),
		pixapitest.WithWebhookURL("https://merchant.example/hooks"),
		pixapitest.WithTimeout(10*time.Second),
		pixapitest.WithSkip("lotecobv", "cob/list"),
		pixapitest.WithPayer(func(ctx context.Context, txid string) error {
			_, err := mockpsp.SimulatePay(ctx, c.HTTPClient(), ts.URL, txid, nil, "")
			return err
		}),
	)
}

func TestNewTxID(t *testing.T) {
	a, b := pixapitest.NewTxID(), pixapitest.NewTxID()
	if a == b || pixapi.ValidateTxID(a) != nil {
		t.Fatalf("NewTxID = %q, %q", a, b)
	}
}

// TestSandbox runs the suite against a real PSP sandbox. It is opt-in:
//
//	PIXAPI_SANDBOX_URL            API root, e.g. https://api-h.psp.example/api/v2
//	PIXAPI_SANDBOX_TOKEN_URL      OAuth2 token endpoint
//	PIXAPI_SANDBOX_CLIENT_ID      client_id
//	PIXAPI_SANDBOX_CLIENT_SECRET  client_secret
//	PIXAPI_SANDBOX_CERT           client certificate (PEM)
//	PIXAPI_SANDBOX_KEY            client private key (PEM)
//	PIXAPI_SANDBOX_CA             optional CA bundle for the PSP's server certificate
//	PIXAPI_SANDBOX_CHAVE          a Pix key registered to the sandbox account
//	PIXAPI_SANDBOX_SCOPES         optional space-separated scopes
//	PIXAPI_SANDBOX_SKIP           optional comma-separated subtests to skip
//
// Settlement tests are skipped: sandboxes simulate payments in
// PSP-specific ways.
func TestSandbox(t *testing.T) {
	base := os.Getenv("PIXAPI_SANDBOX_URL")
	if base == "" {
		t.Skip("PIXAPI_SANDBOX_URL not set")
	}
	tlsCfg, err := pixapi.LoadClientTLS(os.Getenv("PIXAPI_SANDBOX_CERT"), os.Getenv("PIXAPI_SANDBOX_KEY"), os.Getenv("PIXAPI_SANDBOX_CA"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := pixapi.NewClient(base,
		pixapi.WithTLSConfig(tlsCfg),
		pixapi.WithOAuth2(os.Getenv("PIXAPI_SANDBOX_TOKEN_URL"), os.Getenv("PIXAPI_SANDBOX_CLIENT_ID"), os.Getenv("PIXAPI_SANDBOX_CLIENT_SECRET"),
			strings.Fields(os.Getenv("PIXAPI_SANDBOX_SCOPES"))...),
	)
	if err != nil {
		t.Fatal(err)
	}
	opts := []pixapitest.Option{pixapitest.WithChave(os.Getenv("PIXAPI_SANDBOX_CHAVE")), pixapitest.WithRawHTTPClient(c.HTTPClient())}
	if skip := os.Getenv("PIXAPI_SANDBOX_SKIP"); skip != "" {
		opts = append(opts, pixapitest.WithSkip(strings.Split(skip, ",")...))
	}
	pixapitest.Run(t, base, c, opts...)
}
