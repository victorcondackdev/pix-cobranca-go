// Package pixapitest is a conformance suite for API Pix implementations.
//
// Run exercises a PSP through a configured *pixapi.Client and checks the
// behaviors a merchant integration relies on: charge lifecycle and
// revisions, txid uniqueness, problem documents on errors, pagination,
// payload locations, webhook registration, batches and, when a payer is
// available, settlement and refunds.
//
// The suite runs against mockpsp in this module's CI. Point it at a PSP
// sandbox to find out where that PSP differs from the specification (and
// from this client's assumptions); see sandbox_test.go for the opt-in
// environment variables. Checks are limited to behavior the specification
// states clearly; where PSPs legitimately differ (status codes for
// duplicates, refund settlement timing) the suite accepts the documented
// alternatives.
package pixapitest

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/brpay/pix"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

// DefaultChave is a syntactically valid random key used when no key is
// configured. Real PSPs only accept keys owned by the merchant.
const DefaultChave = "2c3e8d1a-5b6f-4a7e-9c0d-1e2f3a4b5c6d"

type config struct {
	chave      string
	payer      func(ctx context.Context, txid string) error
	raw        *http.Client
	webhookURL string
	skip       map[string]bool
	timeout    time.Duration
}

// Option configures Run.
type Option func(*config)

// WithChave sets the Pix key used in charges. It must belong to the
// merchant at a real PSP.
func WithChave(k string) Option { return func(c *config) { c.chave = k } }

// WithPayer enables the settlement tests: pay must cause the charge with
// the given txid to be paid in full (mockpsp exposes POST
// /simulate/pay/{txid}; sandboxes usually have an equivalent).
func WithPayer(pay func(ctx context.Context, txid string) error) Option {
	return func(c *config) { c.payer = pay }
}

// WithRawHTTPClient enables the authentication tests, which send requests
// without a token. Give it the mTLS-enabled client (client.HTTPClient()).
func WithRawHTTPClient(hc *http.Client) Option { return func(c *config) { c.raw = hc } }

// WithWebhookURL sets the URL registered by the webhook test. Default
// "https://webhook.example.com/pix-cobranca-go".
func WithWebhookURL(u string) Option { return func(c *config) { c.webhookURL = u } }

// WithSkip skips the named subtests, e.g. "lotecobv" for a PSP that does
// not offer batches. Names match the first path element or a full name
// ("cob/patch").
func WithSkip(names ...string) Option {
	return func(c *config) {
		for _, n := range names {
			c.skip[strings.TrimSpace(n)] = true
		}
	}
}

// WithTimeout bounds each subtest. Default 30 seconds.
func WithTimeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// Run executes the conformance suite against the PSP at baseURL (the API
// root also configured in client).
func Run(t *testing.T, baseURL string, client *pixapi.Client, opts ...Option) {
	t.Helper()
	cfg := config{chave: DefaultChave, webhookURL: "https://webhook.example.com/pix-cobranca-go", skip: map[string]bool{}, timeout: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	s := &suite{cfg: cfg, base: strings.TrimSuffix(baseURL, "/"), c: client, start: time.Now().Add(-time.Minute)}
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, context.Context)
	}{
		{"auth/no-token", s.authNoToken},
		{"cob/put-get", s.cobPutGet},
		{"cob/duplicate-txid", s.cobDuplicate},
		{"cob/post", s.cobPost},
		{"cob/patch", s.cobPatch},
		{"cob/cancel", s.cobCancel},
		{"cob/not-found", s.cobNotFound},
		{"cob/invalid", s.cobInvalid},
		{"cob/list", s.cobList},
		{"cobv/lifecycle", s.cobvLifecycle},
		{"loc/lifecycle", s.locLifecycle},
		{"webhook/lifecycle", s.webhookLifecycle},
		{"lotecobv/lifecycle", s.loteLifecycle},
		{"pix/pay-and-refund", s.payAndRefund},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			group, _, _ := strings.Cut(tc.name, "/")
			if cfg.skip[group] || cfg.skip[tc.name] {
				t.Skip("skipped by configuration")
			}
			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			defer cancel()
			tc.fn(t, ctx)
		})
	}
}

type suite struct {
	cfg   config
	base  string
	c     *pixapi.Client
	start time.Time
}

// NewTxID returns a random 30-character txid.
func NewTxID() string {
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

func (s *suite) cobRequest(cents int64) pixapi.CobRequest {
	return pixapi.CobRequest{
		Calendario:         &pixapi.Calendario{Expiracao: 3600},
		Devedor:            &pixapi.Devedor{CPF: "12345678909", Nome: "Fulano de Tal"},
		Valor:              pixapi.Valor{Original: pixapi.Amount(cents)},
		Chave:              s.cfg.chave,
		SolicitacaoPagador: "pix-cobranca-go conformance",
		InfoAdicionais:     []pixapi.InfoAdicional{{Nome: "pedido", Valor: "42"}},
	}
}

func (s *suite) createCob(t *testing.T, ctx context.Context, cents int64) *pixapi.Cob {
	t.Helper()
	txid := NewTxID()
	cob, err := s.c.CreateCob(ctx, txid, s.cobRequest(cents))
	if err != nil {
		t.Fatalf("CreateCob: %v", err)
	}
	if cob.TxID != txid {
		t.Fatalf("txid = %q, want %q", cob.TxID, txid)
	}
	return cob
}

// requireProblem checks err is an *pixapi.Error with the given status and a
// well-formed problem document.
func requireProblem(t *testing.T, err error, status int) *pixapi.Error {
	t.Helper()
	var pe *pixapi.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *pixapi.Error with status %d", err, err, status)
	}
	if pe.StatusCode != status {
		t.Fatalf("status = %d, want %d (%v)", pe.StatusCode, status, err)
	}
	if pe.Type == "" || pe.Title == "" {
		t.Errorf("response is not an RFC 7807 problem with type and title: %v", err)
	}
	if pe.Problem.Status != 0 && pe.Problem.Status != status {
		t.Errorf("problem status = %d, HTTP status = %d", pe.Problem.Status, status)
	}
	return pe
}

func checkBRCode(t *testing.T, payload, location string) {
	t.Helper()
	if payload == "" {
		t.Log("pixCopiaECola absent; BR Code checks skipped")
		return
	}
	bc, err := pix.Decode(payload)
	if err != nil {
		t.Fatalf("pixCopiaECola does not decode: %v", err)
	}
	if !bc.Dynamic {
		t.Errorf("pixCopiaECola is not a dynamic BR Code")
	}
	if location != "" && bc.URL != strings.TrimPrefix(location, "https://") {
		t.Errorf("BR Code URL = %q, want location %q", bc.URL, location)
	}
	for _, is := range pix.Lint(payload) {
		if is.Severity == pix.SeverityError {
			t.Errorf("pixCopiaECola lint: %s", is)
		}
	}
}

func (s *suite) authNoToken(t *testing.T, ctx context.Context) {
	if s.cfg.raw == nil {
		t.Skip("no raw HTTP client configured (WithRawHTTPClient)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/cob/"+NewTxID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.cfg.raw.Do(req)
	if err != nil {
		t.Fatalf("request without token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("request without token: status %d, want 401 or 403", resp.StatusCode)
	}
}

func (s *suite) cobPutGet(t *testing.T, ctx context.Context) {
	cob := s.createCob(t, ctx, 1234)
	if cob.Status != pixapi.StatusAtiva {
		t.Errorf("status = %s, want ATIVA", cob.Status)
	}
	if cob.Revisao != 0 {
		t.Errorf("revisao = %d, want 0", cob.Revisao)
	}
	if cob.Valor.Original != 1234 {
		t.Errorf("valor.original = %s, want 12.34", cob.Valor.Original)
	}
	if cob.Calendario.Criacao == nil || cob.Calendario.Expiracao != 3600 {
		t.Errorf("calendario = %+v, want criacao set and expiracao 3600", cob.Calendario)
	}
	loc := cob.Location
	if loc == "" && cob.Loc != nil {
		loc = cob.Loc.Location
	}
	if loc == "" {
		t.Errorf("no location in the created charge")
	}
	checkBRCode(t, cob.PixCopiaECola, loc)

	got, err := s.c.GetCob(ctx, cob.TxID)
	if err != nil {
		t.Fatalf("GetCob: %v", err)
	}
	if got.TxID != cob.TxID || got.Valor != cob.Valor || got.Chave != cob.Chave || got.Status != cob.Status {
		t.Errorf("GetCob = %+v, want %+v", got, cob)
	}
	if got.Devedor == nil || got.Devedor.CPF != "12345678909" {
		t.Errorf("devedor = %+v", got.Devedor)
	}
}

func (s *suite) cobDuplicate(t *testing.T, ctx context.Context) {
	cob := s.createCob(t, ctx, 100)
	_, err := s.c.CreateCob(ctx, cob.TxID, s.cobRequest(100))
	if err == nil {
		t.Fatal("second PUT with the same txid succeeded")
	}
	var pe *pixapi.Error
	if !errors.As(err, &pe) || (pe.StatusCode != http.StatusBadRequest && pe.StatusCode != http.StatusConflict) {
		t.Fatalf("duplicate txid: %v, want 400 or 409", err)
	}
}

func (s *suite) cobPost(t *testing.T, ctx context.Context) {
	cob, err := s.c.CreateCob(ctx, "", s.cobRequest(500))
	if err != nil {
		t.Fatalf("POST /cob: %v", err)
	}
	if err := pixapi.ValidateTxID(cob.TxID); err != nil {
		t.Errorf("generated txid %q: %v", cob.TxID, err)
	}
}

func (s *suite) cobPatch(t *testing.T, ctx context.Context) {
	cob := s.createCob(t, ctx, 1000)
	sol := "valor revisado"
	got, err := s.c.UpdateCob(ctx, cob.TxID, pixapi.CobPatch{Valor: &pixapi.Valor{Original: 1500}, SolicitacaoPagador: &sol})
	if err != nil {
		t.Fatalf("UpdateCob: %v", err)
	}
	if got.Revisao != cob.Revisao+1 {
		t.Errorf("revisao = %d, want %d", got.Revisao, cob.Revisao+1)
	}
	if got.Valor.Original != 1500 || got.SolicitacaoPagador != sol {
		t.Errorf("patched charge = %+v", got)
	}
	if got.Chave != cob.Chave || got.Devedor == nil {
		t.Errorf("PATCH dropped fields it did not mention: %+v", got)
	}
	old, err := s.c.GetCobRevision(ctx, cob.TxID, 0)
	if err != nil {
		t.Fatalf("GetCobRevision(0): %v", err)
	}
	if old.Valor.Original != 1000 {
		t.Errorf("revision 0 valor = %s, want 10.00", old.Valor.Original)
	}
}

func (s *suite) cobCancel(t *testing.T, ctx context.Context) {
	cob := s.createCob(t, ctx, 700)
	got, err := s.c.CancelCob(ctx, cob.TxID)
	if err != nil {
		t.Fatalf("CancelCob: %v", err)
	}
	if got.Status != pixapi.StatusRemovidaPeloUsuarioRecebedor {
		t.Fatalf("status = %s, want REMOVIDA_PELO_USUARIO_RECEBEDOR", got.Status)
	}
	_, err = s.c.UpdateCob(ctx, cob.TxID, pixapi.CobPatch{Valor: &pixapi.Valor{Original: 800}})
	if err == nil {
		t.Fatal("PATCH of a removed charge succeeded")
	}
	requireProblem(t, err, http.StatusBadRequest)
}

func (s *suite) cobNotFound(t *testing.T, ctx context.Context) {
	_, err := s.c.GetCob(ctx, NewTxID())
	if !errors.Is(err, pixapi.ErrNotFound) {
		t.Fatalf("GetCob(unknown) = %v, want ErrNotFound", err)
	}
	requireProblem(t, err, http.StatusNotFound)
}

func (s *suite) cobInvalid(t *testing.T, ctx context.Context) {
	// Bypass client-side validation to see how the PSP reports errors.
	body := map[string]any{"valor": map[string]string{"original": "12,34"}, "chave": s.cfg.chave}
	err := s.c.Do(ctx, http.MethodPut, "/cob/"+NewTxID(), nil, body, nil)
	if err == nil {
		t.Fatal("PUT /cob with a malformed amount succeeded")
	}
	requireProblem(t, err, http.StatusBadRequest)

	body = map[string]any{"valor": map[string]string{"original": "1.00"}, "chave": s.cfg.chave}
	err = s.c.Do(ctx, http.MethodPut, "/cob/tooShort", nil, body, nil)
	if err == nil {
		t.Fatal("PUT /cob with a 8-character txid succeeded")
	}
	requireProblem(t, err, http.StatusBadRequest)
}

func (s *suite) cobList(t *testing.T, ctx context.Context) {
	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		want[s.createCob(t, ctx, int64(100+i)).TxID] = true
	}
	it := s.c.ListCob(ctx, pixapi.CobQuery{PageQuery: pixapi.PageQuery{Inicio: s.start, Fim: time.Now().Add(time.Minute), ItensPorPagina: 1}})
	pages := map[int]bool{}
	for it.Next() {
		delete(want, it.Value().TxID)
		pages[it.Paginacao().PaginaAtual] = true
	}
	if err := it.Err(); err != nil {
		t.Fatalf("ListCob: %v", err)
	}
	if len(want) > 0 {
		t.Errorf("charges missing from the listing: %v", want)
	}
	if len(pages) < 3 {
		t.Errorf("visited %d pages with itensPorPagina=1, want at least 3", len(pages))
	}
	if p := it.Paginacao(); p.QuantidadeTotalDeItens < 3 || p.ItensPorPagina != 1 {
		t.Errorf("paginacao = %+v", p)
	}
}

func (s *suite) cobvRequest() pixapi.CobVRequest {
	return pixapi.CobVRequest{
		Calendario: pixapi.CalendarioCobV{DataDeVencimento: time.Now().AddDate(0, 0, 10).Format(time.DateOnly), ValidadeAposVencimento: 30},
		Devedor:    &pixapi.Devedor{CNPJ: "12345678000195", Nome: "Empresa de Serviços SA"},
		Valor: pixapi.ValorCobV{
			Original: 12345,
			Multa:    &pixapi.Modalidade{Modalidade: 2, ValorPerc: "2.00"},
			Juros:    &pixapi.Modalidade{Modalidade: 2, ValorPerc: "0.30"},
			Desconto: &pixapi.Desconto{Modalidade: 1, DescontoDataFixa: []pixapi.DescontoDataFixa{
				{Data: time.Now().AddDate(0, 0, 5).Format(time.DateOnly), ValorPerc: "10.00"},
			}},
		},
		Chave:              s.cfg.chave,
		SolicitacaoPagador: "boleto Pix",
	}
}

func (s *suite) cobvLifecycle(t *testing.T, ctx context.Context) {
	txid := NewTxID()
	req := s.cobvRequest()
	cobv, err := s.c.CreateCobV(ctx, txid, req)
	if err != nil {
		t.Fatalf("CreateCobV: %v", err)
	}
	if cobv.Status != pixapi.StatusAtiva || cobv.TxID != txid {
		t.Errorf("cobv = %+v", cobv)
	}
	if cobv.Calendario.DataDeVencimento != req.Calendario.DataDeVencimento {
		t.Errorf("dataDeVencimento = %q, want %q", cobv.Calendario.DataDeVencimento, req.Calendario.DataDeVencimento)
	}
	if cobv.Valor.Multa == nil || cobv.Valor.Multa.ValorPerc != "2.00" || cobv.Valor.Juros == nil || cobv.Valor.Desconto == nil {
		t.Errorf("multa/juros/desconto not echoed: %+v", cobv.Valor)
	}
	if cobv.Loc != nil {
		checkBRCode(t, cobv.PixCopiaECola, cobv.Loc.Location)
	}
	got, err := s.c.GetCobV(ctx, txid)
	if err != nil {
		t.Fatalf("GetCobV: %v", err)
	}
	if got.Valor.Original != 12345 {
		t.Errorf("GetCobV valor = %s", got.Valor.Original)
	}
	patched, err := s.c.UpdateCobV(ctx, txid, pixapi.CobVPatch{Valor: &pixapi.ValorCobV{Original: 20000}})
	if err != nil {
		t.Fatalf("UpdateCobV: %v", err)
	}
	if patched.Revisao != 1 || patched.Valor.Original != 20000 {
		t.Errorf("patched cobv = revisao %d valor %s", patched.Revisao, patched.Valor.Original)
	}
	if _, err := s.c.GetCobV(ctx, NewTxID()); !errors.Is(err, pixapi.ErrNotFound) {
		t.Errorf("GetCobV(unknown) = %v, want ErrNotFound", err)
	}
}

func (s *suite) locLifecycle(t *testing.T, ctx context.Context) {
	loc, err := s.c.CreateLoc(ctx, pixapi.TipoCobImediata)
	if err != nil {
		t.Fatalf("CreateLoc: %v", err)
	}
	if loc.ID == 0 && loc.Location == "" {
		t.Fatalf("CreateLoc = %+v", loc)
	}
	if strings.HasPrefix(loc.Location, "https://") {
		t.Errorf("location %q carries a scheme; the API Pix stores it without one", loc.Location)
	}
	req := s.cobRequest(990)
	req.Loc = &pixapi.LocRef{ID: loc.ID}
	cob, err := s.c.CreateCob(ctx, NewTxID(), req)
	if err != nil {
		t.Fatalf("CreateCob with loc: %v", err)
	}
	if cob.Loc == nil || cob.Loc.ID != loc.ID {
		t.Fatalf("charge loc = %+v, want id %d", cob.Loc, loc.ID)
	}
	checkBRCode(t, cob.PixCopiaECola, loc.Location)
	got, err := s.c.GetLoc(ctx, loc.ID)
	if err != nil {
		t.Fatalf("GetLoc: %v", err)
	}
	if got.TxID != cob.TxID {
		t.Errorf("loc txid = %q, want %q", got.TxID, cob.TxID)
	}
	un, err := s.c.UnlinkLoc(ctx, loc.ID)
	if err != nil {
		t.Fatalf("UnlinkLoc: %v", err)
	}
	if un.TxID != "" {
		t.Errorf("loc still linked to %q", un.TxID)
	}
	found := false
	it := s.c.ListLoc(ctx, pixapi.LocQuery{PageQuery: pixapi.PageQuery{Inicio: s.start, Fim: time.Now().Add(time.Minute)}})
	for it.Next() {
		found = found || it.Value().ID == loc.ID
	}
	if err := it.Err(); err != nil {
		t.Fatalf("ListLoc: %v", err)
	}
	if !found {
		t.Errorf("loc %d missing from ListLoc", loc.ID)
	}
}

func (s *suite) webhookLifecycle(t *testing.T, ctx context.Context) {
	if err := s.c.PutWebhook(ctx, s.cfg.chave, s.cfg.webhookURL); err != nil {
		t.Fatalf("PutWebhook: %v", err)
	}
	wh, err := s.c.GetWebhook(ctx, s.cfg.chave)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if wh.WebhookURL != s.cfg.webhookURL {
		t.Errorf("webhookUrl = %q, want %q", wh.WebhookURL, s.cfg.webhookURL)
	}
	found := false
	it := s.c.ListWebhooks(ctx, pixapi.PageQuery{})
	for it.Next() {
		found = found || it.Value().WebhookURL == s.cfg.webhookURL
	}
	if err := it.Err(); err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if !found {
		t.Errorf("webhook missing from ListWebhooks")
	}
	if err := s.c.DeleteWebhook(ctx, s.cfg.chave); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if _, err := s.c.GetWebhook(ctx, s.cfg.chave); !errors.Is(err, pixapi.ErrNotFound) {
		t.Errorf("GetWebhook after delete = %v, want ErrNotFound", err)
	}
}

func (s *suite) loteLifecycle(t *testing.T, ctx context.Context) {
	good, bad := NewTxID(), NewTxID()
	badReq := s.cobvRequest()
	badReq.Calendario.DataDeVencimento = "2026-02-30"
	id := time.Now().UnixNano() % 1_000_000_000
	// The bad item is sent raw: the client would refuse the whole batch.
	body := map[string]any{
		"descricao": "conformance",
		"cobsv": []any{
			pixapi.LoteCobVRequestItem{CobVRequest: s.cobvRequest(), TxID: good},
			pixapi.LoteCobVRequestItem{CobVRequest: badReq, TxID: bad},
		},
	}
	if err := s.c.Do(ctx, http.MethodPut, "/lotecobv/"+itoa(id), nil, body, nil); err != nil {
		t.Fatalf("PUT /lotecobv: %v", err)
	}
	var lote *pixapi.LoteCobV
	deadline := time.Now().Add(s.cfg.timeout / 2)
	for {
		var err error
		lote, err = s.c.GetLoteCobV(ctx, id)
		if err != nil {
			t.Fatalf("GetLoteCobV: %v", err)
		}
		pending := false
		for _, it := range lote.CobsV {
			pending = pending || it.Status == pixapi.LoteEmProcessamento
		}
		if !pending || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	status := map[string]pixapi.LoteItemStatus{}
	for _, it := range lote.CobsV {
		status[it.TxID] = it.Status
		if it.Status == pixapi.LoteNegada && it.Problema == nil {
			t.Errorf("NEGADA item %s without problema", it.TxID)
		}
	}
	if status[good] != pixapi.LoteCriada {
		t.Errorf("valid item status = %q, want CRIADA", status[good])
	}
	if status[bad] != pixapi.LoteNegada {
		t.Errorf("invalid item status = %q, want NEGADA", status[bad])
	}
	if _, err := s.c.GetCobV(ctx, good); err != nil {
		t.Errorf("charge created by the batch: %v", err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (s *suite) payAndRefund(t *testing.T, ctx context.Context) {
	if s.cfg.payer == nil {
		t.Skip("no payer configured (WithPayer)")
	}
	cob := s.createCob(t, ctx, 5000)
	if err := s.cfg.payer(ctx, cob.TxID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	var paid *pixapi.Cob
	for deadline := time.Now().Add(s.cfg.timeout / 2); ; {
		var err error
		if paid, err = s.c.GetCob(ctx, cob.TxID); err != nil {
			t.Fatalf("GetCob: %v", err)
		}
		if paid.Status == pixapi.StatusConcluida || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if paid.Status != pixapi.StatusConcluida {
		t.Fatalf("status after payment = %s, want CONCLUIDA", paid.Status)
	}
	if len(paid.Pix) != 1 {
		t.Fatalf("pix in paid charge = %d, want 1", len(paid.Pix))
	}
	p := paid.Pix[0]
	if err := pixapi.ValidateEndToEndID(p.EndToEndID); err != nil {
		t.Errorf("endToEndId: %v", err)
	}
	if p.Valor != 5000 || p.TxID != cob.TxID {
		t.Errorf("pix = %+v", p)
	}
	got, err := s.c.GetPix(ctx, p.EndToEndID)
	if err != nil {
		t.Fatalf("GetPix: %v", err)
	}
	if got.Valor != p.Valor {
		t.Errorf("GetPix valor = %s", got.Valor)
	}
	found := false
	it := s.c.ListPix(ctx, pixapi.PixQuery{PageQuery: pixapi.PageQuery{Inicio: s.start, Fim: time.Now().Add(time.Minute)}, TxID: cob.TxID})
	for it.Next() {
		found = found || it.Value().EndToEndID == p.EndToEndID
	}
	if err := it.Err(); err != nil {
		t.Fatalf("ListPix: %v", err)
	}
	if !found {
		t.Errorf("pix missing from ListPix")
	}

	d, err := s.c.CreateDevolucao(ctx, p.EndToEndID, "dev1", pixapi.DevolucaoRequest{Valor: 2000})
	if err != nil {
		t.Fatalf("CreateDevolucao: %v", err)
	}
	if d.ID != "dev1" || d.Valor != 2000 || d.RtrID == "" {
		t.Errorf("devolucao = %+v", d)
	}
	if d.Status != pixapi.DevolucaoEmProcessamento && d.Status != pixapi.DevolucaoDevolvido {
		t.Errorf("devolucao status = %s", d.Status)
	}
	if _, err := s.c.GetDevolucao(ctx, p.EndToEndID, "dev1"); err != nil {
		t.Errorf("GetDevolucao: %v", err)
	}
	_, err = s.c.CreateDevolucao(ctx, p.EndToEndID, "dev2", pixapi.DevolucaoRequest{Valor: 3001})
	if err == nil {
		t.Fatal("refund above the remaining amount succeeded")
	}
	requireProblem(t, err, http.StatusBadRequest)
	if _, err := s.c.GetDevolucao(ctx, p.EndToEndID, "nope"); !errors.Is(err, pixapi.ErrNotFound) {
		t.Errorf("GetDevolucao(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.c.CancelCob(ctx, cob.TxID); err == nil {
		t.Errorf("cancelling a CONCLUIDA charge succeeded")
	}
}
