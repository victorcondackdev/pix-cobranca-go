package pixapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

func seg(s string) string { return "/" + url.PathEscape(s) }

func setBool(v url.Values, k string, b *bool) {
	if b != nil {
		v.Set(k, strconv.FormatBool(*b))
	}
}

func setStr(v url.Values, k, s string) {
	if s != "" {
		v.Set(k, s)
	}
}

// ---- cob -----------------------------------------------------------------

// CreateCob creates an immediate charge. With a txid it calls
// PUT /cob/{txid}, the recommended form, as it makes the call safely
// retryable; with an empty txid it calls POST /cob and the PSP assigns one.
//
// The request is validated first, and its key and payer documents are
// normalized (the caller's value is not modified).
func (c *Client) CreateCob(ctx context.Context, txid string, req CobRequest) (*Cob, error) {
	if req.Devedor != nil {
		d := *req.Devedor
		req.Devedor = &d
	}
	if txid != "" {
		if err := ValidateTxID(txid); err != nil {
			return nil, invalidParam("txid", err)
		}
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out Cob
	method, path := http.MethodPost, "/cob"
	if txid != "" {
		method, path = http.MethodPut, "/cob"+seg(txid)
	}
	if err := c.Do(ctx, method, path, nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCob fetches an immediate charge by txid (GET /cob/{txid}).
func (c *Client) GetCob(ctx context.Context, txid string) (*Cob, error) {
	return c.GetCobRevision(ctx, txid, -1)
}

// GetCobRevision fetches a specific revision of an immediate charge
// (GET /cob/{txid}?revisao=n). A negative revision means the latest.
func (c *Client) GetCobRevision(ctx context.Context, txid string, revisao int) (*Cob, error) {
	if err := validateTxIDRef(txid); err != nil {
		return nil, invalidParam("txid", err)
	}
	var q url.Values
	if revisao >= 0 {
		q = url.Values{"revisao": {strconv.Itoa(revisao)}}
	}
	var out Cob
	if err := c.Do(ctx, http.MethodGet, "/cob"+seg(txid), q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateCob revises an immediate charge (PATCH /cob/{txid}). Each
// successful update increments the charge's revisao.
func (c *Client) UpdateCob(ctx context.Context, txid string, patch CobPatch) (*Cob, error) {
	if err := validateTxIDRef(txid); err != nil {
		return nil, invalidParam("txid", err)
	}
	if patch.Devedor != nil {
		d := *patch.Devedor
		patch.Devedor = &d
	}
	if err := patch.Validate(); err != nil {
		return nil, err
	}
	var out Cob
	if err := c.Do(ctx, http.MethodPatch, "/cob"+seg(txid), nil, &patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelCob removes an active immediate charge by setting its status to
// REMOVIDA_PELO_USUARIO_RECEBEDOR.
func (c *Client) CancelCob(ctx context.Context, txid string) (*Cob, error) {
	return c.UpdateCob(ctx, txid, CobPatch{Status: StatusRemovidaPeloUsuarioRecebedor})
}

// CobQuery filters GET /cob and GET /cobv.
type CobQuery struct {
	PageQuery
	CPF, CNPJ        string
	LocationPresente *bool
	Status           Status
}

func (q CobQuery) values() url.Values {
	v := url.Values{}
	setStr(v, "cpf", q.CPF)
	setStr(v, "cnpj", q.CNPJ)
	setBool(v, "locationPresente", q.LocationPresente)
	setStr(v, "status", string(q.Status))
	return v
}

type cobList struct {
	Parametros Parametros `json:"parametros"`
	Cobs       []Cob      `json:"cobs"`
}

// ListCob iterates over immediate charges created between q.Inicio and
// q.Fim (GET /cob).
func (c *Client) ListCob(ctx context.Context, q CobQuery) *Iterator[Cob] {
	return list(c, ctx, "/cob", q.PageQuery, q.values(), true, func(e *cobList) ([]Cob, Parametros) { return e.Cobs, e.Parametros })
}

// ---- cobv ----------------------------------------------------------------

// CreateCobV creates a charge with due date (PUT /cobv/{txid}). There is no
// POST form: the txid is always chosen by the receiver.
func (c *Client) CreateCobV(ctx context.Context, txid string, req CobVRequest) (*CobV, error) {
	if err := ValidateTxID(txid); err != nil {
		return nil, invalidParam("txid", err)
	}
	if req.Devedor != nil {
		d := *req.Devedor
		req.Devedor = &d
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out CobV
	if err := c.Do(ctx, http.MethodPut, "/cobv"+seg(txid), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCobV fetches a charge with due date (GET /cobv/{txid}).
func (c *Client) GetCobV(ctx context.Context, txid string) (*CobV, error) {
	if err := validateTxIDRef(txid); err != nil {
		return nil, invalidParam("txid", err)
	}
	var out CobV
	if err := c.Do(ctx, http.MethodGet, "/cobv"+seg(txid), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateCobV revises a charge with due date (PATCH /cobv/{txid}).
func (c *Client) UpdateCobV(ctx context.Context, txid string, patch CobVPatch) (*CobV, error) {
	if err := validateTxIDRef(txid); err != nil {
		return nil, invalidParam("txid", err)
	}
	if patch.Devedor != nil {
		d := *patch.Devedor
		patch.Devedor = &d
	}
	if err := patch.Validate(); err != nil {
		return nil, err
	}
	var out CobV
	if err := c.Do(ctx, http.MethodPatch, "/cobv"+seg(txid), nil, &patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type cobvList struct {
	Parametros Parametros `json:"parametros"`
	Cobs       []CobV     `json:"cobs"`
}

// ListCobV iterates over charges with due date (GET /cobv).
func (c *Client) ListCobV(ctx context.Context, q CobQuery) *Iterator[CobV] {
	return list(c, ctx, "/cobv", q.PageQuery, q.values(), true, func(e *cobvList) ([]CobV, Parametros) { return e.Cobs, e.Parametros })
}

// ---- lotecobv ------------------------------------------------------------

// CreateLoteCobV submits a batch of charges with due date
// (PUT /lotecobv/{id}). The PSP answers 202 and processes the batch
// asynchronously; poll GetLoteCobV for each charge's status.
func (c *Client) CreateLoteCobV(ctx context.Context, id int64, req LoteCobVRequest) error {
	return c.putLote(ctx, http.MethodPut, id, req)
}

// UpdateLoteCobV resubmits or revises charges of an existing batch
// (PATCH /lotecobv/{id}).
func (c *Client) UpdateLoteCobV(ctx context.Context, id int64, req LoteCobVRequest) error {
	return c.putLote(ctx, http.MethodPatch, id, req)
}

func (c *Client) putLote(ctx context.Context, method string, id int64, req LoteCobVRequest) error {
	if id < 0 {
		return invalidParam("id", errNegativeID)
	}
	items := make([]LoteCobVRequestItem, len(req.CobsV))
	for i, it := range req.CobsV {
		if it.Devedor != nil {
			d := *it.Devedor
			it.Devedor = &d
		}
		items[i] = it
	}
	req.CobsV = items
	if err := req.Validate(); err != nil {
		return err
	}
	return c.Do(ctx, method, "/lotecobv/"+strconv.FormatInt(id, 10), nil, &req, nil)
}

// GetLoteCobV fetches a batch and the status of its charges.
func (c *Client) GetLoteCobV(ctx context.Context, id int64) (*LoteCobV, error) {
	var out LoteCobV
	if err := c.Do(ctx, http.MethodGet, "/lotecobv/"+strconv.FormatInt(id, 10), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type loteList struct {
	Parametros Parametros `json:"parametros"`
	Lotes      []LoteCobV `json:"lotes"`
}

// ListLoteCobV iterates over batches (GET /lotecobv).
func (c *Client) ListLoteCobV(ctx context.Context, q PageQuery) *Iterator[LoteCobV] {
	return list(c, ctx, "/lotecobv", q, nil, true, func(e *loteList) ([]LoteCobV, Parametros) { return e.Lotes, e.Parametros })
}

// ---- pix -----------------------------------------------------------------

// GetPix fetches a received Pix by EndToEndId (GET /pix/{e2eid}).
func (c *Client) GetPix(ctx context.Context, e2eid string) (*Pix, error) {
	if err := ValidateEndToEndID(e2eid); err != nil {
		return nil, invalidParam("e2eid", err)
	}
	var out Pix
	if err := c.Do(ctx, http.MethodGet, "/pix"+seg(e2eid), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PixQuery filters GET /pix.
type PixQuery struct {
	PageQuery
	TxID              string
	TxIDPresente      *bool
	DevolucaoPresente *bool
	CPF, CNPJ         string
}

type pixList struct {
	Parametros Parametros `json:"parametros"`
	Pix        []Pix      `json:"pix"`
}

// ListPix iterates over received Pix (GET /pix).
func (c *Client) ListPix(ctx context.Context, q PixQuery) *Iterator[Pix] {
	v := url.Values{}
	setStr(v, "txid", q.TxID)
	setBool(v, "txIdPresente", q.TxIDPresente)
	setBool(v, "devolucaoPresente", q.DevolucaoPresente)
	setStr(v, "cpf", q.CPF)
	setStr(v, "cnpj", q.CNPJ)
	return list(c, ctx, "/pix", q.PageQuery, v, true, func(e *pixList) ([]Pix, Parametros) { return e.Pix, e.Parametros })
}

// CreateDevolucao requests a refund of a received Pix
// (PUT /pix/{e2eid}/devolucao/{id}). The id is chosen by the receiver,
// which makes the call safely retryable. The refund is usually
// EM_PROCESSAMENTO when this returns; poll GetDevolucao or wait for the
// webhook.
func (c *Client) CreateDevolucao(ctx context.Context, e2eid, id string, req DevolucaoRequest) (*Devolucao, error) {
	if err := ValidateEndToEndID(e2eid); err != nil {
		return nil, invalidParam("e2eid", err)
	}
	if err := ValidateDevolucaoID(id); err != nil {
		return nil, invalidParam("id", err)
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out Devolucao
	if err := c.Do(ctx, http.MethodPut, "/pix"+seg(e2eid)+"/devolucao"+seg(id), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDevolucao fetches a refund (GET /pix/{e2eid}/devolucao/{id}).
func (c *Client) GetDevolucao(ctx context.Context, e2eid, id string) (*Devolucao, error) {
	if err := ValidateEndToEndID(e2eid); err != nil {
		return nil, invalidParam("e2eid", err)
	}
	if err := ValidateDevolucaoID(id); err != nil {
		return nil, invalidParam("id", err)
	}
	var out Devolucao
	if err := c.Do(ctx, http.MethodGet, "/pix"+seg(e2eid)+"/devolucao"+seg(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- webhook -------------------------------------------------------------

func normKey(chave string) (string, error) {
	var v validator
	v.chave("chave", &chave)
	return chave, v.err()
}

// PutWebhook registers (or replaces) the webhook for a Pix key
// (PUT /webhook/{chave}). The URL must be https; the PSP calls it with
// mTLS, see package webhook.
func (c *Client) PutWebhook(ctx context.Context, chave, webhookURL string) error {
	chave, err := normKey(chave)
	if err != nil {
		return err
	}
	if err := validateWebhookURL(webhookURL); err != nil {
		return err
	}
	return c.Do(ctx, http.MethodPut, "/webhook"+seg(chave), nil, map[string]string{"webhookUrl": webhookURL}, nil)
}

// GetWebhook fetches the webhook registered for a key (GET /webhook/{chave}).
func (c *Client) GetWebhook(ctx context.Context, chave string) (*Webhook, error) {
	chave, err := normKey(chave)
	if err != nil {
		return nil, err
	}
	var out Webhook
	if err := c.Do(ctx, http.MethodGet, "/webhook"+seg(chave), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteWebhook removes the webhook of a key (DELETE /webhook/{chave}).
func (c *Client) DeleteWebhook(ctx context.Context, chave string) error {
	chave, err := normKey(chave)
	if err != nil {
		return err
	}
	return c.Do(ctx, http.MethodDelete, "/webhook"+seg(chave), nil, nil, nil)
}

type webhookList struct {
	Parametros Parametros `json:"parametros"`
	Webhooks   []Webhook  `json:"webhooks"`
}

// ListWebhooks iterates over webhook registrations (GET /webhook). The
// time range is optional for this endpoint: leave Inicio and Fim zero to
// list all.
func (c *Client) ListWebhooks(ctx context.Context, q PageQuery) *Iterator[Webhook] {
	return list(c, ctx, "/webhook", q, nil, false, func(e *webhookList) ([]Webhook, Parametros) { return e.Webhooks, e.Parametros })
}

// ---- loc -----------------------------------------------------------------

// CreateLoc reserves a payload location (POST /loc), to be linked to a
// charge later through CobRequest.Loc.
func (c *Client) CreateLoc(ctx context.Context, tipo TipoCob) (*Loc, error) {
	if tipo != TipoCobImediata && tipo != TipoCobVencimento {
		return nil, &ValidationError{Violacoes: []Violacao{{Propriedade: "tipoCob", Razao: "must be cob or cobv"}}}
	}
	var out Loc
	if err := c.Do(ctx, http.MethodPost, "/loc", nil, map[string]TipoCob{"tipoCob": tipo}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetLoc fetches a payload location (GET /loc/{id}).
func (c *Client) GetLoc(ctx context.Context, id int64) (*Loc, error) {
	var out Loc
	if err := c.Do(ctx, http.MethodGet, "/loc/"+strconv.FormatInt(id, 10), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UnlinkLoc detaches the charge linked to a payload location
// (DELETE /loc/{id}/txid), so the location can serve another charge.
func (c *Client) UnlinkLoc(ctx context.Context, id int64) (*Loc, error) {
	var out Loc
	if err := c.Do(ctx, http.MethodDelete, "/loc/"+strconv.FormatInt(id, 10)+"/txid", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LocQuery filters GET /loc.
type LocQuery struct {
	PageQuery
	TxIDPresente *bool
	TipoCob      TipoCob
}

type locList struct {
	Parametros Parametros `json:"parametros"`
	Loc        []Loc      `json:"loc"`
}

// ListLoc iterates over payload locations (GET /loc).
func (c *Client) ListLoc(ctx context.Context, q LocQuery) *Iterator[Loc] {
	v := url.Values{}
	setBool(v, "txIdPresente", q.TxIDPresente)
	setStr(v, "tipoCob", string(q.TipoCob))
	return list(c, ctx, "/loc", q.PageQuery, v, true, func(e *locList) ([]Loc, Parametros) { return e.Loc, e.Parametros })
}
