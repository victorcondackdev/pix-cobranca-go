package mockpsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/victorcondackdev/brpay/pix"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

const (
	titlePixNotFound       = "Pix não encontrado para o e2eid informado."
	titleDevolucaoInvalida = "Devolução inválida."
	titleDevolucaoNotFound = "Devolução não encontrada."
	titleWebhookInvalido   = "Webhook inválido."
	titleWebhookNotFound   = "Webhook não encontrado."
)

// brt is the offset used to decide when a charge with due date stops being
// payable. Brasília time has had no daylight saving since 2019.
var brt = time.FixedZone("BRT", -3*60*60)

func pixNotFound() (int, any) {
	return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypePixNaoEncontrado, titlePixNotFound, "Não existe Pix recebido com o e2eid informado.")
}

func copyPix(p *pixapi.Pix) pixapi.Pix {
	c := *p
	c.Devolucoes = append([]pixapi.Devolucao(nil), p.Devolucoes...)
	return c
}

func (s *Server) getPix(r *http.Request) (int, any) {
	p := s.pix[r.PathValue("e2eid")]
	if p == nil {
		return pixNotFound()
	}
	c := copyPix(p)
	return http.StatusOK, &c
}

func (s *Server) chargeDevedor(txid string) *pixapi.Devedor {
	if revs := s.cobs[txid]; revs != nil {
		return revs[len(revs)-1].Devedor
	}
	if revs := s.cobvs[txid]; revs != nil {
		return revs[len(revs)-1].Devedor
	}
	return nil
}

func (s *Server) listPix(r *http.Request) (int, any) {
	inicio, fim, p := timeRange(r, true)
	if p != nil {
		return p.Status, p
	}
	q := r.URL.Query()
	var items []pixapi.Pix
	for _, px := range s.pix {
		if !inRange(px.Horario, inicio, fim) {
			continue
		}
		if t := q.Get("txid"); t != "" && px.TxID != t {
			continue
		}
		if tp := q.Get("txIdPresente"); tp != "" && (px.TxID != "") != (tp == "true") {
			continue
		}
		if dp := q.Get("devolucaoPresente"); dp != "" && (len(px.Devolucoes) > 0) != (dp == "true") {
			continue
		}
		if (q.Get("cpf") != "" || q.Get("cnpj") != "") && !matchDoc(s.chargeDevedor(px.TxID), q.Get("cpf"), q.Get("cnpj")) {
			continue
		}
		items = append(items, copyPix(px))
	}
	sortByTime(items, func(p pixapi.Pix) time.Time { return p.Horario }, func(p pixapi.Pix) string { return p.EndToEndID })
	page, params, p := paginate(r, items, inicio, fim)
	if p != nil {
		return p.Status, p
	}
	if page == nil {
		page = []pixapi.Pix{}
	}
	return http.StatusOK, map[string]any{"parametros": params, "pix": page}
}

func devolucaoInvalida(detail string, v ...pixapi.Violacao) (int, any) {
	return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeDevolucaoOperacaoInvalida, titleDevolucaoInvalida, detail, v...)
}

func (s *Server) putDevolucao(r *http.Request) (int, any) {
	px := s.pix[r.PathValue("e2eid")]
	if px == nil {
		return pixNotFound()
	}
	id := r.PathValue("id")
	if err := pixapi.ValidateDevolucaoID(id); err != nil {
		return devolucaoInvalida("id inválido.", viol("id", err.Error()))
	}
	var req pixapi.DevolucaoRequest
	if err := decode(r, &req); err != nil {
		return devolucaoInvalida("Corpo da requisição inválido: " + err.Error())
	}
	if err := req.Validate(); err != nil {
		return http.StatusBadRequest, validationProblem(pixapi.TypeDevolucaoOperacaoInvalida, titleDevolucaoInvalida, err)
	}
	if req.Natureza == "" {
		req.Natureza = pixapi.NaturezaOriginal
	}
	for _, d := range px.Devolucoes {
		if d.ID != id {
			continue
		}
		// Replaying the same request returns the existing refund, which
		// keeps PUT idempotent; a different body for the same id is an
		// error.
		if d.Valor == req.Valor && d.Natureza == req.Natureza {
			c := d
			return http.StatusCreated, &c
		}
		return devolucaoInvalida("O id de devolução informado já foi utilizado com outros dados.", viol("id", "já utilizado"))
	}
	if px.Refunded()+req.Valor > px.Valor {
		return devolucaoInvalida(fmt.Sprintf("O valor da devolução excede o valor disponível (%s).", px.Valor-px.Refunded()), viol("valor", "excede o valor disponível"))
	}
	now := s.now()
	rtr, err := pix.NewEndToEndID(s.cfg.ISPB, now, nil)
	if err != nil {
		return http.StatusInternalServerError, problem(http.StatusInternalServerError, pixapi.TypeErroInternoDoServidor, "Erro interno do servidor.", "")
	}
	d := pixapi.Devolucao{
		ID: id, RtrID: "D" + rtr[1:], Valor: req.Valor, Natureza: req.Natureza, Descricao: req.Descricao,
		Horario: pixapi.DevolucaoHorario{Solicitacao: &now}, Status: pixapi.DevolucaoEmProcessamento,
	}
	px.Devolucoes = append(px.Devolucoes, d)
	idx := len(px.Devolucoes) - 1
	if s.cfg.RefundSettleDelay <= 0 {
		s.settleLocked(px, idx)
	} else {
		e2e := px.EndToEndID
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTimer(s.cfg.RefundSettleDelay)
			defer t.Stop()
			select {
			case <-t.C:
			case <-s.ctx.Done():
				return
			}
			s.mu.Lock()
			if p := s.pix[e2e]; p != nil {
				s.settleLocked(p, idx)
			}
			out := s.outbox
			s.outbox = nil
			s.mu.Unlock()
			for _, o := range out {
				s.deliver(o)
			}
		}()
	}
	c := px.Devolucoes[idx]
	s.syncChargeLocked(px)
	return http.StatusCreated, &c
}

func (s *Server) settleLocked(px *pixapi.Pix, idx int) {
	now := s.now()
	d := &px.Devolucoes[idx]
	d.Status = pixapi.DevolucaoDevolvido
	d.Horario.Liquidacao = &now
	s.syncChargeLocked(px)
	s.enqueueLocked(px)
}

// syncChargeLocked copies px into the pix list of its charge.
func (s *Server) syncChargeLocked(px *pixapi.Pix) {
	if revs := s.cobs[px.TxID]; revs != nil {
		c := &revs[len(revs)-1]
		for i := range c.Pix {
			if c.Pix[i].EndToEndID == px.EndToEndID {
				c.Pix[i] = copyPix(px)
			}
		}
	}
	if revs := s.cobvs[px.TxID]; revs != nil {
		c := &revs[len(revs)-1]
		for i := range c.Pix {
			if c.Pix[i].EndToEndID == px.EndToEndID {
				c.Pix[i] = copyPix(px)
			}
		}
	}
}

func (s *Server) getDevolucao(r *http.Request) (int, any) {
	px := s.pix[r.PathValue("e2eid")]
	if px == nil {
		return pixNotFound()
	}
	for _, d := range px.Devolucoes {
		if d.ID == r.PathValue("id") {
			c := d
			return http.StatusOK, &c
		}
	}
	return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeDevolucaoNaoEncontrada, titleDevolucaoNotFound, "Não existe devolução com o id informado.")
}

// ---- webhook registration --------------------------------------------------

func (s *Server) pathKey(r *http.Request) (string, *pixapi.Problem) {
	k, err := normalizeKey(r.PathValue("chave"))
	if err != nil {
		return "", problem(http.StatusBadRequest, pixapi.TypeWebhookOperacaoInvalida, titleWebhookInvalido, "Chave inválida.", viol("chave", "inválida"))
	}
	return k, nil
}

func (s *Server) putWebhook(r *http.Request) (int, any) {
	k, p := s.pathKey(r)
	if p != nil {
		return p.Status, p
	}
	if !s.ownsKey(k) {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeWebhookOperacaoInvalida, titleWebhookInvalido, detailChaveNotOwned, viol("chave", "não pertence ao recebedor"))
	}
	var req struct {
		WebhookURL string `json:"webhookUrl"`
	}
	if err := decode(r, &req); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeWebhookOperacaoInvalida, titleWebhookInvalido, "Corpo da requisição inválido: "+err.Error())
	}
	u, err := url.Parse(req.WebhookURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(s.cfg.AllowHTTPWebhooks && u.Scheme == "http")) {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeWebhookOperacaoInvalida, titleWebhookInvalido, "webhookUrl deve ser uma URL https absoluta.", viol("webhookUrl", "inválida"))
	}
	now := s.now()
	s.webhooks[k] = &pixapi.Webhook{WebhookURL: req.WebhookURL, Chave: k, Criacao: &now}
	return http.StatusOK, nil
}

func webhookNotFound() (int, any) {
	return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeWebhookNaoEncontrado, titleWebhookNotFound, "Não existe webhook para a chave informada.")
}

func (s *Server) getWebhook(r *http.Request) (int, any) {
	k, p := s.pathKey(r)
	if p != nil {
		return p.Status, p
	}
	wh := s.webhooks[k]
	if wh == nil {
		return webhookNotFound()
	}
	c := *wh
	return http.StatusOK, &c
}

func (s *Server) deleteWebhook(r *http.Request) (int, any) {
	k, p := s.pathKey(r)
	if p != nil {
		return p.Status, p
	}
	if s.webhooks[k] == nil {
		return webhookNotFound()
	}
	delete(s.webhooks, k)
	return http.StatusNoContent, nil
}

func (s *Server) listWebhooks(r *http.Request) (int, any) {
	inicio, fim, p := timeRange(r, false)
	if p != nil {
		return p.Status, p
	}
	var items []pixapi.Webhook
	for _, wh := range s.webhooks {
		if inRange(*wh.Criacao, inicio, fim) {
			items = append(items, *wh)
		}
	}
	sortByTime(items, func(w pixapi.Webhook) time.Time { return *w.Criacao }, func(w pixapi.Webhook) string { return w.Chave })
	page, params, p := paginate(r, items, inicio, fim)
	if p != nil {
		return p.Status, p
	}
	if page == nil {
		page = []pixapi.Webhook{}
	}
	return http.StatusOK, map[string]any{"parametros": params, "webhooks": page}
}

// ---- simulation ------------------------------------------------------------

// PayRequest is the optional body of POST /simulate/pay/{txid}.
type PayRequest struct {
	// Valor is the amount paid. Default: valor.original. For immediate
	// charges that do not allow changing the amount it must match.
	Valor *pixapi.Amount `json:"valor,omitempty"`
	// InfoPagador is the payer's message.
	InfoPagador string `json:"infoPagador,omitempty"`
}

func decodeOptional(r *http.Request, v any) error {
	err := decode(r, v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// simulatePay settles a charge as if it had been paid. The mock does not
// compute fines, interest or discounts of charges with due date: pass the
// amount to settle in the body, or valor.original is used.
func (s *Server) simulatePay(r *http.Request) (int, any) {
	txid := r.PathValue("txid")
	var req PayRequest
	if err := decodeOptional(r, &req); err != nil {
		return http.StatusBadRequest, invalidRequest("Corpo da requisição inválido: " + err.Error())
	}
	now := s.now()
	var chave string
	var valor pixapi.Amount
	var appendPix func(pixapi.Pix)
	switch {
	case s.cobs[txid] != nil:
		revs := s.cobs[txid]
		c := &revs[len(revs)-1]
		if c.Status != pixapi.StatusAtiva {
			return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "A cobrança não está ATIVA (status "+string(c.Status)+").")
		}
		if now.After(c.Calendario.Criacao.Add(time.Duration(c.Calendario.Expiracao) * time.Second)) {
			return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "A cobrança está expirada.")
		}
		valor = c.Valor.Original
		if req.Valor != nil {
			if c.Valor.ModalidadeAlteracao != 1 && *req.Valor != valor {
				return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "A cobrança não permite alterar o valor.")
			}
			valor = *req.Valor
		}
		chave = c.Chave
		appendPix = func(p pixapi.Pix) { c.Status = pixapi.StatusConcluida; c.Pix = append(c.Pix, p) }
	case s.cobvs[txid] != nil:
		revs := s.cobvs[txid]
		c := &revs[len(revs)-1]
		if c.Status != pixapi.StatusAtiva {
			return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "A cobrança não está ATIVA (status "+string(c.Status)+").")
		}
		due, _ := time.ParseInLocation(time.DateOnly, c.Calendario.DataDeVencimento, brt)
		if !now.Before(due.AddDate(0, 0, c.Calendario.ValidadeAposVencimento+1)) {
			return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "A cobrança não pode mais ser paga.")
		}
		valor = c.Valor.Original
		if req.Valor != nil {
			valor = *req.Valor
		}
		chave = c.Chave
		appendPix = func(p pixapi.Pix) { c.Status = pixapi.StatusConcluida; c.Pix = append(c.Pix, p) }
	default:
		return cobNotFound()
	}
	if valor <= 0 {
		return http.StatusBadRequest, invalidRequest("O valor pago deve ser positivo.", viol("valor", "deve ser positivo"))
	}
	e2e, err := pix.NewEndToEndID(s.cfg.PayerISPB, now, nil)
	if err != nil {
		return http.StatusInternalServerError, problem(http.StatusInternalServerError, pixapi.TypeErroInternoDoServidor, "Erro interno do servidor.", "")
	}
	p := &pixapi.Pix{EndToEndID: e2e, TxID: txid, Valor: valor, Chave: chave, Horario: now, InfoPagador: req.InfoPagador}
	s.pix[e2e] = p
	appendPix(copyPix(p))
	s.enqueueLocked(p)
	c := copyPix(p)
	return http.StatusCreated, &c
}

// simulateRemove marks a charge REMOVIDA_PELO_PSP.
func (s *Server) simulateRemove(r *http.Request) (int, any) {
	txid := r.PathValue("txid")
	if revs := s.cobs[txid]; revs != nil {
		c := &revs[len(revs)-1]
		c.Status = pixapi.StatusRemovidaPeloPSP
		cc := *c
		return http.StatusOK, &cc
	}
	if revs := s.cobvs[txid]; revs != nil {
		c := &revs[len(revs)-1]
		c.Status = pixapi.StatusRemovidaPeloPSP
		cc := *c
		return http.StatusOK, &cc
	}
	return cobNotFound()
}

// ---- webhook delivery ------------------------------------------------------

// enqueueLocked queues a notification for px if its key has a webhook.
// Deliveries start after the state lock is released.
func (s *Server) enqueueLocked(px *pixapi.Pix) {
	wh := s.webhooks[px.Chave]
	if wh == nil {
		return
	}
	body, err := json.Marshal(map[string][]pixapi.Pix{"pix": {copyPix(px)}})
	if err != nil {
		s.cfg.Logger.Error("encode webhook", "error", err)
		return
	}
	s.outbox = append(s.outbox, outgoing{url: strings.TrimSuffix(wh.WebhookURL, "/") + "/pix", e2e: px.EndToEndID, body: body})
}

func (s *Server) deliver(o outgoing) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		delay := s.cfg.WebhookBackoff
		for attempt := 1; attempt <= s.cfg.WebhookAttempts; attempt++ {
			status, err := s.post(o)
			d := Delivery{URL: o.url, EndToEndID: o.e2e, Attempt: attempt, Status: status, At: time.Now()}
			if err != nil {
				d.Err = err.Error()
			}
			s.mu.Lock()
			s.deliveries = append(s.deliveries, d)
			s.mu.Unlock()
			if err == nil && status >= 200 && status < 300 {
				return
			}
			s.cfg.Logger.Warn("webhook delivery failed", "url", o.url, "attempt", attempt, "status", status, "error", d.Err)
			if attempt == s.cfg.WebhookAttempts {
				return
			}
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-s.ctx.Done():
				t.Stop()
				return
			}
			delay *= 2
		}
	}()
}

func (s *Server) post(o outgoing) (int, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(o.body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.WebhookClient.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return resp.StatusCode, nil
}
