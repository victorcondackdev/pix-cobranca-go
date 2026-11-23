package mockpsp_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-cobranca-go/internal/devpki"
	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
	"github.com/victorcondackdev/pix-cobranca-go/pixapitest"
	"github.com/victorcondackdev/pix-cobranca-go/webhook"
)

const chave = pixapitest.DefaultChave

func (e *env) token(t *testing.T, hc *http.Client, form url.Values) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if form.Get("client_id") == "" {
		req.SetBasicAuth("dev-client", "dev-secret")
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) bearer(t *testing.T, scope string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	if scope != "" {
		form.Set("scope", scope)
	}
	code, out := e.token(t, e.hc, form)
	if code != 200 {
		t.Fatalf("token: %d %v", code, out)
	}
	return out["access_token"].(string)
}

// raw sends an API request with the given token and returns status and
// decoded body.
func (e *env) raw(t *testing.T, hc *http.Client, tok, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		if s, ok := body.(string); ok {
			b = []byte(s)
		}
		rd = strings.NewReader(string(b))
	}
	u := e.ts.URL + path
	if !strings.HasPrefix(path, "/simulate") && !strings.HasPrefix(path, "/healthz") {
		u = e.ts.URL + e.psp.BasePath() + path
	}
	req, _ := http.NewRequest(method, u, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func tlsClient(pool *devpki.Set, certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool.CA.Pool(), Certificates: certs}}}
}

func TestTokenEndpoint(t *testing.T) {
	e := newEnv(t, mockpsp.Config{})
	noCert := tlsClient(e.pki)
	cases := []struct {
		name string
		hc   *http.Client
		form url.Values
		code int
		err  string
	}{
		{"no certificate", noCert, url.Values{"grant_type": {"client_credentials"}}, 401, "invalid_client"},
		{"bad grant", e.hc, url.Values{"grant_type": {"password"}}, 400, "unsupported_grant_type"},
		{"bad secret", e.hc, url.Values{"grant_type": {"client_credentials"}, "client_id": {"dev-client"}, "client_secret": {"nope"}}, 401, "invalid_client"},
		{"unknown scope", e.hc, url.Values{"grant_type": {"client_credentials"}, "scope": {"cob.read admin"}}, 400, "invalid_scope"},
	}
	for _, c := range cases {
		code, out := e.token(t, c.hc, c.form)
		if code != c.code || out["error"] != c.err {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
	code, out := e.token(t, e.hc, url.Values{"grant_type": {"client_credentials"}, "client_id": {"dev-client"}, "client_secret": {"dev-secret"}, "scope": {"cob.read"}})
	if code != 200 || out["scope"] != "cob.read" || out["token_type"] != "Bearer" {
		t.Fatalf("form credentials: %d %v", code, out)
	}
	if e.psp.TokenRequests() != 1 {
		t.Errorf("TokenRequests = %d", e.psp.TokenRequests())
	}
	tok := out["access_token"].(string)

	if code, out := e.raw(t, e.hc, tok, "POST", "/loc", map[string]string{"tipoCob": "cob"}); code != 403 || out["type"] != pixapi.TypeAcessoNegado {
		t.Errorf("missing scope: %d %v", code, out)
	}
	if code, _ := e.raw(t, e.hc, tok, "GET", "/cob/"+pixapitest.NewTxID(), nil); code != 404 {
		t.Errorf("granted scope: %d", code)
	}
	if code, _ := e.raw(t, noCert, tok, "GET", "/cob/"+pixapitest.NewTxID(), nil); code != 401 {
		t.Errorf("token without certificate: %d", code)
	}
	// The token is bound to the certificate that obtained it.
	if code, out := e.raw(t, tlsClient(e.pki, e.pki.WebhookClient.TLS), tok, "GET", "/cob/"+pixapitest.NewTxID(), nil); code != 401 || !strings.Contains(fmt.Sprint(out["detail"]), "outro certificado") {
		t.Errorf("token with another certificate: %d %v", code, out)
	}
	if code, _ := e.raw(t, e.hc, "", "GET", "/cob/x", nil); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := e.raw(t, e.hc, "forged", "GET", "/cob/x", nil); code != 401 {
		t.Errorf("forged token: %d", code)
	}
	if code, out := e.raw(t, noCert, "", "GET", "/healthz", nil); code != 200 || out["status"] != "ok" {
		t.Errorf("healthz: %d %v", code, out)
	}
}

func TestTokenExpiry(t *testing.T) {
	var offset atomic.Int64
	e := newEnv(t, mockpsp.Config{TokenTTL: time.Minute, Now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }})
	tok := e.bearer(t, "")
	if code, _ := e.raw(t, e.hc, tok, "GET", "/cob/"+pixapitest.NewTxID(), nil); code != 404 {
		t.Fatalf("fresh token: %d", code)
	}
	offset.Store(int64(2 * time.Minute))
	if code, _ := e.raw(t, e.hc, tok, "GET", "/cob/"+pixapitest.NewTxID(), nil); code != 401 {
		t.Fatalf("expired token: %d", code)
	}
	// The SDK recovers transparently: its cached token is rejected, it
	// fetches a new one and retries.
	if _, err := e.client.GetCob(context.Background(), pixapitest.NewTxID()); !errors.Is(err, pixapi.ErrNotFound) {
		t.Fatalf("SDK after expiry: %v", err)
	}
}

func TestPlainHTTPAndNew(t *testing.T) {
	psp, err := mockpsp.New(mockpsp.Config{BasePath: "/", Keys: []string{"+55 21 98765-4321"}})
	if err != nil {
		t.Fatal(err)
	}
	defer psp.Close()
	if psp.BasePath() != "" {
		t.Errorf("BasePath = %q", psp.BasePath())
	}
	for _, cfg := range []mockpsp.Config{
		{PublicURL: "::"},
		{Keys: []string{"not-a-key"}},
		{MerchantName: strings.Repeat("N", 40)},
	} {
		if _, err := mockpsp.New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestCobEdges(t *testing.T) {
	e := newEnv(t, mockpsp.Config{Keys: []string{chave}})
	tok := e.bearer(t, "")
	ctx := context.Background()
	txid := pixapitest.NewTxID()
	body := map[string]any{"valor": map[string]string{"original": "10.00"}, "chave": chave, "devedor": map[string]string{"cpf": "12345678909", "nome": "F"}}
	if code, _ := e.raw(t, e.hc, tok, "PUT", "/cob/"+txid, body); code != 201 {
		t.Fatalf("create: %d", code)
	}
	other := map[string]any{"valor": map[string]string{"original": "10.00"}, "chave": "outra@example.com"}
	checks := []struct {
		name, method, path string
		body               any
		code               int
		typ                string
	}{
		{"bad json", "PUT", "/cob/" + pixapitest.NewTxID(), "{", 400, pixapi.TypeCobOperacaoInvalida},
		{"schema", "PUT", "/cob/" + pixapitest.NewTxID(), map[string]any{"valor": map[string]string{"original": "0.00"}}, 400, pixapi.TypeCobOperacaoInvalida},
		{"key not owned", "POST", "/cob", other, 400, pixapi.TypeCobOperacaoInvalida},
		{"loc missing", "PUT", "/cob/" + pixapitest.NewTxID(), map[string]any{"valor": map[string]string{"original": "1.00"}, "chave": chave, "loc": map[string]int{"id": 999}}, 400, pixapi.TypeCobOperacaoInvalida},
		{"revisao bad", "GET", "/cob/" + txid + "?revisao=x", nil, 400, pixapi.TypeRequisicaoInvalida},
		{"revisao missing", "GET", "/cob/" + txid + "?revisao=5", nil, 404, pixapi.TypeCobNaoEncontrado},
		{"patch missing", "PATCH", "/cob/" + pixapitest.NewTxID(), map[string]any{}, 404, pixapi.TypeCobNaoEncontrado},
		{"patch bad json", "PATCH", "/cob/" + txid, "[", 400, pixapi.TypeCobOperacaoInvalida},
		{"patch schema", "PATCH", "/cob/" + txid, map[string]any{"status": "CONCLUIDA"}, 400, pixapi.TypeCobOperacaoInvalida},
		{"patch key not owned", "PATCH", "/cob/" + txid, map[string]any{"chave": "outra@example.com"}, 400, pixapi.TypeCobOperacaoInvalida},
		{"list no range", "GET", "/cob", nil, 400, pixapi.TypeRequisicaoInvalida},
		{"list bad range", "GET", "/cob?inicio=ontem&fim=hoje", nil, 400, pixapi.TypeRequisicaoInvalida},
		{"list inverted", "GET", "/cob?inicio=2026-02-01T00:00:00Z&fim=2026-01-01T00:00:00Z", nil, 400, pixapi.TypeRequisicaoInvalida},
		{"list bad page", "GET", "/cob?inicio=2026-01-01T00:00:00Z&fim=2027-01-01T00:00:00Z&paginacao.paginaAtual=-1", nil, 400, pixapi.TypeRequisicaoInvalida},
		{"list bad size", "GET", "/cob?inicio=2026-01-01T00:00:00Z&fim=2027-01-01T00:00:00Z&paginacao.itensPorPagina=0", nil, 400, pixapi.TypeRequisicaoInvalida},
	}
	for _, c := range checks {
		code, out := e.raw(t, e.hc, tok, c.method, c.path, c.body)
		if code != c.code || out["type"] != c.typ || out["correlationId"] == "" {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}

	// Locations: wrong type, already linked, moving a charge to another one.
	locV, _ := e.client.CreateLoc(ctx, pixapi.TipoCobVencimento)
	req := pixapi.CobRequest{Valor: pixapi.Valor{Original: 100}, Chave: chave, Loc: &pixapi.LocRef{ID: locV.ID}}
	if _, err := e.client.CreateCob(ctx, pixapitest.NewTxID(), req); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("cob on a cobv location: %v", err)
	}
	cob, _ := e.client.GetCob(ctx, txid)
	req.Loc = &pixapi.LocRef{ID: cob.Loc.ID}
	if _, err := e.client.CreateCob(ctx, pixapitest.NewTxID(), req); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("cob on a linked location: %v", err)
	}
	loc2, _ := e.client.CreateLoc(ctx, pixapi.TipoCobImediata)
	moved, err := e.client.UpdateCob(ctx, txid, pixapi.CobPatch{Loc: &pixapi.LocRef{ID: loc2.ID}, Calendario: &pixapi.Calendario{Expiracao: 60}, Devedor: &pixapi.Devedor{CNPJ: "12345678000195", Nome: "E"}, InfoAdicionais: []pixapi.InfoAdicional{{Nome: "a", Valor: "b"}}})
	if err != nil || moved.Loc.ID != loc2.ID || moved.Calendario.Expiracao != 60 || moved.Devedor.CNPJ == "" {
		t.Fatalf("move location: %+v %v", moved, err)
	}
	if old, _ := e.client.GetLoc(ctx, cob.Loc.ID); old.TxID != "" {
		t.Errorf("old location still linked to %s", old.TxID)
	}
	if _, err := e.client.UpdateCob(ctx, txid, pixapi.CobPatch{Loc: &pixapi.LocRef{ID: 999}}); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("move to missing location: %v", err)
	}

	// Filters.
	now := time.Now()
	q := pixapi.CobQuery{PageQuery: pixapi.PageQuery{Inicio: now.Add(-time.Hour), Fim: now.Add(time.Hour)}}
	count := func(q pixapi.CobQuery) int {
		items, err := e.client.ListCob(ctx, q).All()
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	all := count(q)
	q.CNPJ = "12345678000195"
	if n := count(q); n != 1 {
		t.Errorf("cnpj filter: %d", n)
	}
	q.CNPJ, q.Status = "", pixapi.StatusConcluida
	if n := count(q); n != 0 {
		t.Errorf("status filter: %d", n)
	}
	no := false
	q.Status, q.LocationPresente = "", &no
	if n := count(q); n != 0 || all == 0 {
		t.Errorf("locationPresente filter: %d of %d", n, all)
	}
	// Past the last page.
	if code, out := e.raw(t, e.hc, tok, "GET", "/cob?inicio=2026-01-01T00:00:00Z&fim=2030-01-01T00:00:00Z&paginacao.paginaAtual=50", nil); code != 200 || len(out["cobs"].([]any)) != 0 {
		t.Errorf("page past the end: %d %v", code, out)
	}
}

func TestCobVAndLoteEdges(t *testing.T) {
	e := newEnv(t, mockpsp.Config{Recebedor: pixapi.Recebedor{CNPJ: "12345678000195", Nome: "Loja"}, Keys: []string{chave}})
	tok := e.bearer(t, "")
	ctx := context.Background()
	cv := pixapi.CobVRequest{
		Calendario: pixapi.CalendarioCobV{DataDeVencimento: time.Now().AddDate(0, 0, 3).Format(time.DateOnly)},
		Devedor:    &pixapi.Devedor{CPF: "12345678909", Nome: "F"},
		Valor:      pixapi.ValorCobV{Original: 1000},
		Chave:      chave,
	}
	txid := pixapitest.NewTxID()
	c, err := e.client.CreateCobV(ctx, txid, cv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Recebedor == nil || c.Recebedor.Nome != "Loja" || c.Calendario.ValidadeAposVencimento != 30 {
		t.Errorf("cobv = %+v", c)
	}
	if _, err := e.client.CreateCobV(ctx, txid, cv); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("duplicate cobv: %v", err)
	}
	if _, err := e.client.CreateCob(ctx, txid, pixapi.CobRequest{Valor: pixapi.Valor{Original: 1}, Chave: chave}); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("cob reusing a cobv txid: %v", err)
	}
	loc, _ := e.client.CreateLoc(ctx, pixapi.TipoCobVencimento)
	cal := pixapi.CalendarioCobV{DataDeVencimento: time.Now().AddDate(0, 0, 5).Format(time.DateOnly)}
	sol := "nova"
	p, err := e.client.UpdateCobV(ctx, txid, pixapi.CobVPatch{Calendario: &cal, Loc: &pixapi.LocRef{ID: loc.ID}, SolicitacaoPagador: &sol, InfoAdicionais: []pixapi.InfoAdicional{{Nome: "a", Valor: "b"}}, Devedor: &pixapi.Devedor{CPF: "12345678909", Nome: "G"}})
	if err != nil || p.Loc.ID != loc.ID || p.Calendario.ValidadeAposVencimento != 30 || p.Calendario.Criacao == nil {
		t.Fatalf("patch cobv: %+v %v", p, err)
	}
	if _, err := e.client.UpdateCobV(ctx, txid, pixapi.CobVPatch{Chave: "outra@example.com"}); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("key not owned: %v", err)
	}
	if _, err := e.client.UpdateCobV(ctx, txid, pixapi.CobVPatch{Loc: &pixapi.LocRef{ID: 999}}); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("missing loc: %v", err)
	}
	if code, _ := e.raw(t, e.hc, tok, "PATCH", "/cobv/"+txid, "{"); code != 400 {
		t.Errorf("bad json: %d", code)
	}
	if code, _ := e.raw(t, e.hc, tok, "PUT", "/cobv/short", map[string]any{}); code != 400 {
		t.Errorf("short txid: %d", code)
	}
	if code, _ := e.raw(t, e.hc, tok, "PUT", "/cobv/"+pixapitest.NewTxID(), "{"); code != 400 {
		t.Errorf("bad json: %d", code)
	}
	if code, _ := e.raw(t, e.hc, tok, "PUT", "/cobv/"+pixapitest.NewTxID(), map[string]any{"valor": map[string]string{"original": "1.00"}, "chave": "outra@example.com", "calendario": map[string]string{"dataDeVencimento": "2030-01-01"}, "devedor": map[string]string{"cpf": "12345678909", "nome": "F"}}); code != 400 {
		t.Errorf("cobv key not owned: %d", code)
	}
	if code, _ := e.raw(t, e.hc, tok, "GET", "/cobv?inicio=x", nil); code != 400 {
		t.Errorf("list bad range: %d", code)
	}
	if code, _ := e.raw(t, e.hc, tok, "GET", "/cobv?inicio=2026-01-01T00:00:00Z&fim=2030-01-01T00:00:00Z&status=ATIVA&cpf=12345678909&locationPresente=true&paginacao.itensPorPagina=5000", nil); code != 400 {
		t.Errorf("list bad page size: %d", code)
	}
	now := time.Now()
	yes := true
	items, err := e.client.ListCobV(ctx, pixapi.CobQuery{PageQuery: pixapi.PageQuery{Inicio: now.Add(-time.Hour), Fim: now.Add(time.Hour)}, CPF: "12345678909", Status: pixapi.StatusAtiva, LocationPresente: &yes}).All()
	if err != nil || len(items) != 1 {
		t.Errorf("ListCobV = %d, %v", len(items), err)
	}
	if un, err := e.client.UnlinkLoc(ctx, loc.ID); err != nil || un.TxID != "" {
		t.Errorf("unlink cobv loc: %+v %v", un, err)
	}
	if got, _ := e.client.GetCobV(ctx, txid); got.Loc != nil || got.PixCopiaECola != "" {
		t.Errorf("cobv still has a location: %+v", got.Loc)
	}
	if _, err := e.client.UnlinkLoc(ctx, loc.ID); err != nil {
		t.Errorf("unlink twice: %v", err)
	}
	if _, err := e.client.UpdateCobV(ctx, pixapitest.NewTxID(), pixapi.CobVPatch{}); !errors.Is(err, pixapi.ErrNotFound) {
		t.Errorf("patch missing cobv: %v", err)
	}
	if code, _ := e.raw(t, e.hc, tok, "POST", "/simulate/remove/"+txid, nil); code != 200 {
		t.Errorf("remove: %d", code)
	}
	if _, err := e.client.UpdateCobV(ctx, txid, pixapi.CobVPatch{Devedor: &pixapi.Devedor{CPF: "12345678909", Nome: "F"}}); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("patch removed cobv: %v", err)
	}

	// Batches.
	good, bad := pixapitest.NewTxID(), pixapitest.NewTxID()
	badItem := cv
	badItem.Chave = "outra@example.com"
	lote := map[string]any{"cobsv": []any{
		pixapi.LoteCobVRequestItem{CobVRequest: cv, TxID: good},
		pixapi.LoteCobVRequestItem{CobVRequest: badItem, TxID: bad},
		pixapi.LoteCobVRequestItem{CobVRequest: cv, TxID: "short"},
	}}
	if code, _ := e.raw(t, e.hc, tok, "PUT", "/lotecobv/7", lote); code != 202 {
		t.Fatalf("PUT lote: %d", code)
	}
	for _, c := range []struct {
		name, method, path string
		body               any
		code               int
	}{
		{"duplicate id", "PUT", "/lotecobv/7", lote, 400},
		{"bad id", "PUT", "/lotecobv/x", lote, 400},
		{"bad json", "PUT", "/lotecobv/8", "{", 400},
		{"empty", "PUT", "/lotecobv/8", map[string]any{"cobsv": []any{}}, 400},
		{"patch missing", "PATCH", "/lotecobv/99", lote, 404},
		{"patch bad id", "PATCH", "/lotecobv/-1", lote, 400},
		{"patch bad json", "PATCH", "/lotecobv/7", "{", 400},
		{"patch foreign txid", "PATCH", "/lotecobv/7", map[string]any{"cobsv": []any{pixapi.LoteCobVRequestItem{CobVRequest: cv, TxID: pixapitest.NewTxID()}}}, 400},
		{"get missing", "GET", "/lotecobv/99", nil, 404},
		{"get bad id", "GET", "/lotecobv/x", nil, 400},
		{"list bad range", "GET", "/lotecobv", nil, 400},
	} {
		if code, out := e.raw(t, e.hc, tok, c.method, c.path, c.body); code != c.code {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
	// Fix the denied item and revise the created one.
	fixed := cv
	revised := cv
	revised.Valor.Original = 2500
	if err := e.client.UpdateLoteCobV(ctx, 7, pixapi.LoteCobVRequest{Descricao: "revisado", CobsV: []pixapi.LoteCobVRequestItem{{CobVRequest: fixed, TxID: bad}, {CobVRequest: revised, TxID: good}}}); err != nil {
		t.Fatal(err)
	}
	l, err := e.client.GetLoteCobV(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]pixapi.LoteItemStatus{}
	for _, it := range l.CobsV {
		st[it.TxID] = it.Status
	}
	if st[good] != pixapi.LoteCriada || st[bad] != pixapi.LoteCriada || st["short"] != pixapi.LoteNegada || l.Descricao != "revisado" {
		t.Errorf("lote = %+v", l)
	}
	if g, _ := e.client.GetCobV(ctx, good); g.Valor.Original != 2500 || g.Revisao != 1 {
		t.Errorf("revised charge = %s rev %d", g.Valor.Original, g.Revisao)
	}
	// A failing revision is reported on the item.
	_, _ = e.raw(t, e.hc, tok, "POST", "/simulate/remove/"+good, nil)
	if err := e.client.UpdateLoteCobV(ctx, 7, pixapi.LoteCobVRequest{CobsV: []pixapi.LoteCobVRequestItem{{CobVRequest: revised, TxID: good}}}); err != nil {
		t.Fatal(err)
	}
	l, _ = e.client.GetLoteCobV(ctx, 7)
	for _, it := range l.CobsV {
		if it.TxID == good && it.Problema == nil {
			t.Error("failed revision without problema")
		}
	}
	lotes, err := e.client.ListLoteCobV(ctx, pixapi.PageQuery{Inicio: now.Add(-time.Hour), Fim: now.Add(time.Hour)}).All()
	if err != nil || len(lotes) != 1 {
		t.Errorf("ListLoteCobV = %d, %v", len(lotes), err)
	}
}

func TestLocEdges(t *testing.T) {
	e := newEnv(t, mockpsp.Config{})
	tok := e.bearer(t, "")
	for _, c := range []struct {
		name, method, path string
		body               any
		code               int
	}{
		{"bad tipo", "POST", "/loc", map[string]string{"tipoCob": "boleto"}, 400},
		{"bad json", "POST", "/loc", "{", 400},
		{"missing", "GET", "/loc/42", nil, 404},
		{"bad id", "GET", "/loc/abc", nil, 400},
		{"unlink missing", "DELETE", "/loc/42/txid", nil, 404},
		{"unlink bad id", "DELETE", "/loc/abc/txid", nil, 400},
		{"list bad range", "GET", "/loc", nil, 400},
	} {
		if code, out := e.raw(t, e.hc, tok, c.method, c.path, c.body); code != c.code {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
	ctx := context.Background()
	_, _ = e.client.CreateLoc(ctx, pixapi.TipoCobImediata)
	_, _ = e.client.CreateLoc(ctx, pixapi.TipoCobVencimento)
	now := time.Now()
	yes := true
	items, err := e.client.ListLoc(ctx, pixapi.LocQuery{PageQuery: pixapi.PageQuery{Inicio: now.Add(-time.Hour), Fim: now.Add(time.Hour)}, TipoCob: pixapi.TipoCobVencimento}).All()
	if err != nil || len(items) != 1 {
		t.Errorf("tipoCob filter: %d %v", len(items), err)
	}
	items, _ = e.client.ListLoc(ctx, pixapi.LocQuery{PageQuery: pixapi.PageQuery{Inicio: now.Add(-time.Hour), Fim: now.Add(time.Hour)}, TxIDPresente: &yes}).All()
	if len(items) != 0 {
		t.Errorf("txIdPresente filter: %d", len(items))
	}
}

// receiver is a merchant webhook endpoint failing the first n calls.
type receiver struct {
	ts    *httptest.Server
	mu    sync.Mutex
	got   []pixapi.Pix
	fails atomic.Int32
}

func newReceiver(t *testing.T, pki *devpki.Set, fails int32) *receiver {
	rc := &receiver{}
	rc.fails.Store(fails)
	h, err := webhook.New(webhook.Config{ClientCAs: pki.CA.Pool(), OnPix: func(_ context.Context, p pixapi.Pix) error {
		if rc.fails.Load() > 0 {
			rc.fails.Add(-1)
			return errors.New("not now")
		}
		rc.mu.Lock()
		rc.got = append(rc.got, p)
		rc.mu.Unlock()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /hook/pix", h)
	rc.ts = httptest.NewUnstartedServer(mux)
	rc.ts.TLS = webhook.TLSConfig(pki.WebhookServer.TLS, pki.CA.Pool())
	rc.ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	rc.ts.StartTLS()
	t.Cleanup(rc.ts.Close)
	return rc
}

func (rc *receiver) received() []pixapi.Pix {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]pixapi.Pix(nil), rc.got...)
}

func TestPaymentWebhookAndRefunds(t *testing.T) {
	e := newEnv(t, mockpsp.Config{RefundSettleDelay: 20 * time.Millisecond})
	rc := newReceiver(t, e.pki, 2)
	ctx := context.Background()
	if err := e.client.PutWebhook(ctx, chave, rc.ts.URL+"/hook"); err != nil {
		t.Fatal(err)
	}
	cob, err := e.client.CreateCob(ctx, pixapitest.NewTxID(), pixapi.CobRequest{Valor: pixapi.Valor{Original: 1000, ModalidadeAlteracao: 1}, Chave: chave, Devedor: &pixapi.Devedor{CPF: "12345678909", Nome: "F"}})
	if err != nil {
		t.Fatal(err)
	}
	amount := pixapi.Amount(1500)
	p, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cob.TxID, &amount, "valeu")
	if err != nil || p.Valor != 1500 || p.InfoPagador != "valeu" {
		t.Fatalf("pay with changed amount: %+v %v", p, err)
	}
	e.psp.WaitDeliveries()
	got := rc.received()
	if len(got) != 1 || got[0].EndToEndID != p.EndToEndID {
		t.Fatalf("webhook received %v", got)
	}
	ds := e.psp.Deliveries()
	if len(ds) != 3 || ds[0].Status != 500 || ds[2].Status != 200 || ds[2].Attempt != 3 || !strings.HasSuffix(ds[2].URL, "/hook/pix") {
		t.Errorf("deliveries = %+v", ds)
	}

	// Refunds settle asynchronously and notify the webhook again.
	d, err := e.client.CreateDevolucao(ctx, p.EndToEndID, "r1", pixapi.DevolucaoRequest{Valor: 500, Descricao: "troca"})
	if err != nil || d.Status != pixapi.DevolucaoEmProcessamento || d.Natureza != pixapi.NaturezaOriginal || !strings.HasPrefix(d.RtrID, "D") {
		t.Fatalf("refund: %+v %v", d, err)
	}
	if again, err := e.client.CreateDevolucao(ctx, p.EndToEndID, "r1", pixapi.DevolucaoRequest{Valor: 500}); err != nil || again.RtrID != d.RtrID {
		t.Errorf("idempotent replay: %+v %v", again, err)
	}
	if _, err := e.client.CreateDevolucao(ctx, p.EndToEndID, "r1", pixapi.DevolucaoRequest{Valor: 600}); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("conflicting replay: %v", err)
	}
	e.psp.WaitDeliveries()
	got = rc.received()
	if len(got) != 2 || len(got[1].Devolucoes) != 1 || got[1].Devolucoes[0].Status != pixapi.DevolucaoDevolvido {
		t.Fatalf("refund webhook: %+v", got)
	}
	settled, _ := e.client.GetDevolucao(ctx, p.EndToEndID, "r1")
	if settled.Status != pixapi.DevolucaoDevolvido || settled.Horario.Liquidacao == nil {
		t.Errorf("settled refund = %+v", settled)
	}
	if paid, _ := e.client.GetCob(ctx, cob.TxID); len(paid.Pix) != 1 || len(paid.Pix[0].Devolucoes) != 1 {
		t.Errorf("charge pix not updated with refund: %+v", paid.Pix)
	}

	tok := e.bearer(t, "")
	now := time.Now()
	rng := fmt.Sprintf("inicio=%s&fim=%s", url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)), url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339)))
	for _, c := range []struct {
		name, method, path string
		body               any
		code, items        int
	}{
		{"refund missing pix", "PUT", "/pix/E00000000202611221530aB3dE5fG7hJ/devolucao/x", map[string]string{"valor": "1.00"}, 404, -1},
		{"refund bad id", "PUT", "/pix/" + p.EndToEndID + "/devolucao/a-b", map[string]string{"valor": "1.00"}, 400, -1},
		{"refund bad json", "PUT", "/pix/" + p.EndToEndID + "/devolucao/r2", "{", 400, -1},
		{"refund schema", "PUT", "/pix/" + p.EndToEndID + "/devolucao/r2", map[string]string{"valor": "0.00"}, 400, -1},
		{"get refund missing pix", "GET", "/pix/E00000000202611221530aB3dE5fG7hJ/devolucao/x", nil, 404, -1},
		{"get pix missing", "GET", "/pix/E00000000202611221530aB3dE5fG7hJ", nil, 404, -1},
		{"list no range", "GET", "/pix", nil, 400, -1},
		{"list with refunds", "GET", "/pix?" + rng + "&devolucaoPresente=true&txIdPresente=true&cpf=12345678909", nil, 200, 1},
		{"list without refunds", "GET", "/pix?" + rng + "&devolucaoPresente=false", nil, 200, 0},
		{"list other txid", "GET", "/pix?" + rng + "&txid=nope", nil, 200, 0},
		{"list bad page", "GET", "/pix?" + rng + "&paginacao.paginaAtual=x", nil, 400, -1},
	} {
		code, out := e.raw(t, e.hc, tok, c.method, c.path, c.body)
		if code != c.code {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
		if c.items >= 0 && len(out["pix"].([]any)) != c.items {
			t.Errorf("%s: %d items", c.name, len(out["pix"].([]any)))
		}
	}
}

func TestSimulateEdges(t *testing.T) {
	var offset atomic.Int64
	e := newEnv(t, mockpsp.Config{Now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }})
	ctx := context.Background()
	cob, _ := e.client.CreateCob(ctx, pixapitest.NewTxID(), pixapi.CobRequest{Calendario: &pixapi.Calendario{Expiracao: 60}, Valor: pixapi.Valor{Original: 1000}, Chave: chave})
	wrong := pixapi.Amount(999)
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cob.TxID, &wrong, ""); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("wrong amount: %v", err)
	}
	zero := pixapi.Amount(0)
	cobZ, _ := e.client.CreateCob(ctx, pixapitest.NewTxID(), pixapi.CobRequest{Valor: pixapi.Valor{Original: 0, ModalidadeAlteracao: 1}, Chave: chave})
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cobZ.TxID, &zero, ""); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("zero payment: %v", err)
	}
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, pixapitest.NewTxID(), nil, ""); !errors.Is(err, pixapi.ErrNotFound) {
		t.Errorf("unknown txid: %v", err)
	}
	tok := e.bearer(t, "")
	if code, _ := e.raw(t, e.hc, tok, "POST", "/simulate/pay/"+cob.TxID, "{"); code != 400 {
		t.Errorf("bad body: %d", code)
	}
	if code, _ := e.raw(t, e.hc, tok, "POST", "/simulate/remove/"+pixapitest.NewTxID(), nil); code != 404 {
		t.Errorf("remove unknown: %d", code)
	}

	cv := pixapi.CobVRequest{Calendario: pixapi.CalendarioCobV{DataDeVencimento: time.Now().Format(time.DateOnly), ValidadeAposVencimento: 1}, Devedor: &pixapi.Devedor{CPF: "12345678909", Nome: "F"}, Valor: pixapi.ValorCobV{Original: 1000, Juros: &pixapi.Modalidade{Modalidade: 1, ValorPerc: "1.00"}}, Chave: chave}
	cobv, _ := e.client.CreateCobV(ctx, pixapitest.NewTxID(), cv)
	cobv2, _ := e.client.CreateCobV(ctx, pixapitest.NewTxID(), cv)
	withInterest := pixapi.Amount(1010)
	if p, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cobv.TxID, &withInterest, ""); err != nil || p.Valor != 1010 {
		t.Errorf("cobv payment: %+v %v", p, err)
	}
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cobv.TxID, nil, ""); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("paying a CONCLUIDA cobv: %v", err)
	}
	if code, _ := e.raw(t, e.hc, tok, "POST", "/simulate/remove/"+cob.TxID, nil); code != 200 {
		t.Errorf("remove: %d", code)
	}
	if c, _ := e.client.GetCob(ctx, cob.TxID); c.Status != pixapi.StatusRemovidaPeloPSP {
		t.Errorf("status = %s", c.Status)
	}
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cob.TxID, nil, ""); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("paying a removed cob: %v", err)
	}

	// Time travel: the immediate charge expires, the cobv passes its
	// validity.
	cob2, _ := e.client.CreateCob(ctx, pixapitest.NewTxID(), pixapi.CobRequest{Calendario: &pixapi.Calendario{Expiracao: 60}, Valor: pixapi.Valor{Original: 1000}, Chave: chave})
	offset.Store(int64(72 * time.Hour))
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cob2.TxID, nil, ""); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("expired cob: %v", err)
	}
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cobv2.TxID, nil, ""); !errors.Is(err, pixapi.ErrBadRequest) {
		t.Errorf("cobv past validity: %v", err)
	}
	if _, err := mockpsp.SimulatePay(ctx, e.hc, "http://127.0.0.1:1", cob2.TxID, nil, ""); err == nil {
		t.Error("unreachable mock")
	}
}

func TestWebhookRegistrationEdges(t *testing.T) {
	e := newEnv(t, mockpsp.Config{Keys: []string{chave}})
	tok := e.bearer(t, "")
	for _, c := range []struct {
		name, method, path string
		body               any
		code               int
	}{
		{"bad key", "PUT", "/webhook/nope", map[string]string{"webhookUrl": "https://x.example"}, 400},
		{"key not owned", "PUT", "/webhook/outra@example.com", map[string]string{"webhookUrl": "https://x.example"}, 400},
		{"bad json", "PUT", "/webhook/" + chave, "{", 400},
		{"http url", "PUT", "/webhook/" + chave, map[string]string{"webhookUrl": "http://x.example"}, 400},
		{"get bad key", "GET", "/webhook/nope", nil, 400},
		{"get missing", "GET", "/webhook/" + chave, nil, 404},
		{"delete bad key", "DELETE", "/webhook/nope", nil, 400},
		{"delete missing", "DELETE", "/webhook/" + chave, nil, 404},
		{"list bad range", "GET", "/webhook?inicio=x", nil, 400},
		{"list bad page", "GET", "/webhook?paginacao.itensPorPagina=x", nil, 400},
	} {
		if code, out := e.raw(t, e.hc, tok, c.method, c.path, c.body); code != c.code {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
	h, _ := mockpsp.New(mockpsp.Config{AllowHTTPWebhooks: true})
	defer h.Close()
	ts := httptest.NewServer(h.Handler())
	defer ts.Close()
	c, _ := pixapi.NewClient(ts.URL+h.BasePath(), pixapi.WithOAuth2(ts.URL+"/oauth/token", "dev-client", "dev-secret"))
	// The SDK refuses http URLs, so go through Do.
	if err := c.Do(context.Background(), "PUT", "/webhook/"+chave, nil, map[string]string{"webhookUrl": "http://merchant.local/hook"}, nil); err != nil {
		t.Errorf("AllowHTTPWebhooks: %v", err)
	}
}

func TestWebhookDeliveryGivesUp(t *testing.T) {
	e := newEnv(t, mockpsp.Config{WebhookAttempts: 2})
	ctx := context.Background()
	// A receiver whose certificate the PSP does not trust.
	other, _ := devpki.NewSet()
	rc := newReceiver(t, other, 0)
	if err := e.client.PutWebhook(ctx, chave, rc.ts.URL+"/hook/"); err != nil {
		t.Fatal(err)
	}
	cob, _ := e.client.CreateCob(ctx, pixapitest.NewTxID(), pixapi.CobRequest{Valor: pixapi.Valor{Original: 100}, Chave: chave})
	if _, err := mockpsp.SimulatePay(ctx, e.hc, e.ts.URL, cob.TxID, nil, ""); err != nil {
		t.Fatal(err)
	}
	e.psp.WaitDeliveries()
	ds := e.psp.Deliveries()
	if len(ds) != 2 || ds[1].Err == "" || ds[1].Status != 0 {
		t.Errorf("deliveries = %+v", ds)
	}
	if len(rc.received()) != 0 {
		t.Error("untrusted receiver got the notification")
	}
}
