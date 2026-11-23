package mockpsp

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/victorcondackdev/brpay/pix"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

func normalizeKey(k string) (string, error) {
	n, _, err := pix.NormalizeKey(k)
	return n, err
}

const (
	titleCobInvalida    = "Cobrança inválida."
	titleCobNotFound    = "Cobrança não encontrada para o txid informado."
	titleCobVInvalida   = "Cobrança inválida."
	titleCobVNotFound   = "Cobrança não encontrada para o txid informado."
	titleLocInvalida    = "Location inválida."
	titleLocNotFound    = "Location não encontrada para o id informado."
	titleLoteInvalido   = "Lote inválido."
	titleLoteNotFound   = "Lote não encontrado para o id informado."
	detailTxIDUsed      = "O txid informado já foi utilizado."
	detailChaveNotOwned = "A chave informada não pertence ao usuário recebedor."
)

func viol(prop, reason string) pixapi.Violacao {
	return pixapi.Violacao{Propriedade: prop, Razao: reason}
}

func (s *Server) ownsKey(k string) bool { return len(s.keys) == 0 || s.keys[k] }

func (s *Server) newLocLocked(tipo pixapi.TipoCob) *pixapi.Loc {
	s.nextLoc++
	now := s.now()
	l := &pixapi.Loc{ID: s.nextLoc, Location: s.host + "/qr/v2/" + randomHex(16), TipoCob: tipo, Criacao: &now}
	s.locs[l.ID] = l
	return l
}

// linkLocLocked returns the location for a new or revised charge: the one
// referenced by ref (which must exist, match tipo and be free or already
// linked to txid), or a fresh one.
func (s *Server) linkLocLocked(ref *pixapi.LocRef, tipo pixapi.TipoCob, txid, typ string) (*pixapi.Loc, *pixapi.Problem) {
	var l *pixapi.Loc
	if ref == nil {
		l = s.newLocLocked(tipo)
	} else {
		l = s.locs[ref.ID]
		switch {
		case l == nil:
			return nil, problem(http.StatusBadRequest, typ, titleCobInvalida, "Location não encontrada.", viol("loc.id", "inexistente"))
		case l.TipoCob != tipo:
			return nil, problem(http.StatusBadRequest, typ, titleCobInvalida, "Location de tipo incompatível.", viol("loc.id", "tipoCob incompatível"))
		case l.TxID != "" && l.TxID != txid:
			return nil, problem(http.StatusBadRequest, typ, titleCobInvalida, "Location já vinculada a outra cobrança.", viol("loc.id", "já vinculada"))
		}
	}
	l.TxID = txid
	return l, nil
}

func locSnapshot(l *pixapi.Loc) *pixapi.Loc {
	c := *l
	c.TxID = ""
	return &c
}

func (s *Server) brcode(l *pixapi.Loc) string {
	code, err := pix.NewDynamic(pix.DynamicOptions{URL: l.Location, MerchantName: s.cfg.MerchantName, MerchantCity: s.cfg.MerchantCity})
	if err != nil {
		s.cfg.Logger.Error("generate BR Code", "error", err)
		return ""
	}
	return code
}

func pathID(r *http.Request) (int64, *pixapi.Problem) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		return 0, invalidRequest("id inválido.", viol("id", "deve ser um inteiro não negativo"))
	}
	return id, nil
}

// ---- cob -----------------------------------------------------------------

func (s *Server) putCob(r *http.Request) (int, any) {
	txid := r.PathValue("txid")
	if err := pixapi.ValidateTxID(txid); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "txid inválido.", viol("txid", err.Error()))
	}
	return s.createCob(r, txid)
}

func (s *Server) postCob(r *http.Request) (int, any) {
	txid := randomTxID(32)
	for s.cobs[txid] != nil {
		txid = randomTxID(32)
	}
	return s.createCob(r, txid)
}

func (s *Server) createCob(r *http.Request, txid string) (int, any) {
	var req pixapi.CobRequest
	if err := decode(r, &req); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "Corpo da requisição inválido: "+err.Error())
	}
	if err := req.Validate(); err != nil {
		return http.StatusBadRequest, validationProblem(pixapi.TypeCobOperacaoInvalida, titleCobInvalida, err)
	}
	if s.cobs[txid] != nil || s.cobvs[txid] != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, detailTxIDUsed, viol("txid", "já utilizado"))
	}
	if !s.ownsKey(req.Chave) {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, detailChaveNotOwned, viol("chave", "não pertence ao recebedor"))
	}
	l, p := s.linkLocLocked(req.Loc, pixapi.TipoCobImediata, txid, pixapi.TypeCobOperacaoInvalida)
	if p != nil {
		return p.Status, p
	}
	now := s.now()
	exp := pixapi.DefaultExpiracao
	if req.Calendario != nil && req.Calendario.Expiracao > 0 {
		exp = req.Calendario.Expiracao
	}
	cob := pixapi.Cob{
		Calendario:         pixapi.Calendario{Criacao: &now, Expiracao: exp},
		TxID:               txid,
		Revisao:            0,
		Loc:                locSnapshot(l),
		Location:           l.Location,
		Status:             pixapi.StatusAtiva,
		Devedor:            req.Devedor,
		Valor:              req.Valor,
		Chave:              req.Chave,
		SolicitacaoPagador: req.SolicitacaoPagador,
		InfoAdicionais:     req.InfoAdicionais,
		PixCopiaECola:      s.brcode(l),
	}
	s.cobs[txid] = []pixapi.Cob{cob}
	return http.StatusCreated, &cob
}

func cobNotFound() (int, any) {
	return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeCobNaoEncontrado, titleCobNotFound, "Não existe cobrança para o txid informado.")
}

func (s *Server) getCob(r *http.Request) (int, any) {
	revs := s.cobs[r.PathValue("txid")]
	if revs == nil {
		return cobNotFound()
	}
	if v := r.URL.Query().Get("revisao"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return http.StatusBadRequest, invalidRequest("revisao inválida.", viol("revisao", "deve ser um inteiro não negativo"))
		}
		if n >= len(revs) {
			return cobNotFound()
		}
		c := revs[n]
		return http.StatusOK, &c
	}
	c := revs[len(revs)-1]
	return http.StatusOK, &c
}

func (s *Server) patchCob(r *http.Request) (int, any) {
	txid := r.PathValue("txid")
	revs := s.cobs[txid]
	if revs == nil {
		return cobNotFound()
	}
	var p pixapi.CobPatch
	if err := decode(r, &p); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "Corpo da requisição inválido: "+err.Error())
	}
	if err := p.Validate(); err != nil {
		return http.StatusBadRequest, validationProblem(pixapi.TypeCobOperacaoInvalida, titleCobInvalida, err)
	}
	cur := revs[len(revs)-1]
	if cur.Status != pixapi.StatusAtiva {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, "A cobrança não está ATIVA (status "+string(cur.Status)+").")
	}
	if p.Chave != "" && !s.ownsKey(p.Chave) {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobOperacaoInvalida, titleCobInvalida, detailChaveNotOwned, viol("chave", "não pertence ao recebedor"))
	}
	next := cur
	if p.Loc != nil && (cur.Loc == nil || p.Loc.ID != cur.Loc.ID) {
		l, prob := s.linkLocLocked(p.Loc, pixapi.TipoCobImediata, txid, pixapi.TypeCobOperacaoInvalida)
		if prob != nil {
			return prob.Status, prob
		}
		if cur.Loc != nil {
			if old := s.locs[cur.Loc.ID]; old != nil {
				old.TxID = ""
			}
		}
		next.Loc, next.Location, next.PixCopiaECola = locSnapshot(l), l.Location, s.brcode(l)
	}
	if p.Calendario != nil && p.Calendario.Expiracao > 0 {
		next.Calendario.Expiracao = p.Calendario.Expiracao
	}
	if p.Devedor != nil {
		next.Devedor = p.Devedor
	}
	if p.Valor != nil {
		next.Valor = *p.Valor
	}
	if p.Chave != "" {
		next.Chave = p.Chave
	}
	if p.SolicitacaoPagador != nil {
		next.SolicitacaoPagador = *p.SolicitacaoPagador
	}
	if p.InfoAdicionais != nil {
		next.InfoAdicionais = p.InfoAdicionais
	}
	if p.Status != "" {
		next.Status = p.Status
	}
	next.Revisao = cur.Revisao + 1
	s.cobs[txid] = append(revs, next)
	return http.StatusOK, &next
}

func matchDoc(d *pixapi.Devedor, cpf, cnpj string) bool {
	if cpf != "" && (d == nil || d.CPF != cpf) {
		return false
	}
	if cnpj != "" && (d == nil || d.CNPJ != cnpj) {
		return false
	}
	return true
}

func (s *Server) listCob(r *http.Request) (int, any) {
	inicio, fim, p := timeRange(r, true)
	if p != nil {
		return p.Status, p
	}
	q := r.URL.Query()
	var items []pixapi.Cob
	for _, revs := range s.cobs {
		c := revs[len(revs)-1]
		if !inRange(*c.Calendario.Criacao, inicio, fim) || !matchDoc(c.Devedor, q.Get("cpf"), q.Get("cnpj")) {
			continue
		}
		if st := q.Get("status"); st != "" && string(c.Status) != st {
			continue
		}
		if lp := q.Get("locationPresente"); lp != "" && (c.Loc != nil) != (lp == "true") {
			continue
		}
		items = append(items, c)
	}
	sortByTime(items, func(c pixapi.Cob) time.Time { return *c.Calendario.Criacao }, func(c pixapi.Cob) string { return c.TxID })
	page, params, p := paginate(r, items, inicio, fim)
	if p != nil {
		return p.Status, p
	}
	if page == nil {
		page = []pixapi.Cob{}
	}
	return http.StatusOK, map[string]any{"parametros": params, "cobs": page}
}

// ---- cobv ----------------------------------------------------------------

func cobvNotFound() (int, any) {
	return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeCobVNaoEncontrada, titleCobVNotFound, "Não existe cobrança com vencimento para o txid informado.")
}

func (s *Server) putCobV(r *http.Request) (int, any) {
	txid := r.PathValue("txid")
	if err := pixapi.ValidateTxID(txid); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "txid inválido.", viol("txid", err.Error()))
	}
	var req pixapi.CobVRequest
	if err := decode(r, &req); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "Corpo da requisição inválido: "+err.Error())
	}
	c, p := s.createCobVLocked(txid, req)
	if p != nil {
		return p.Status, p
	}
	return http.StatusCreated, c
}

func (s *Server) createCobVLocked(txid string, req pixapi.CobVRequest) (*pixapi.CobV, *pixapi.Problem) {
	if err := req.Validate(); err != nil {
		return nil, validationProblem(pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, err)
	}
	if s.cobs[txid] != nil || s.cobvs[txid] != nil {
		return nil, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, detailTxIDUsed, viol("txid", "já utilizado"))
	}
	if !s.ownsKey(req.Chave) {
		return nil, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, detailChaveNotOwned, viol("chave", "não pertence ao recebedor"))
	}
	l, p := s.linkLocLocked(req.Loc, pixapi.TipoCobVencimento, txid, pixapi.TypeCobVOperacaoInvalida)
	if p != nil {
		return nil, p
	}
	now := s.now()
	cal := req.Calendario
	cal.Criacao = &now
	if cal.ValidadeAposVencimento == 0 {
		cal.ValidadeAposVencimento = pixapi.DefaultValidadeAposVencimento
	}
	var rec *pixapi.Recebedor
	if s.cfg.Recebedor != (pixapi.Recebedor{}) {
		rc := s.cfg.Recebedor
		rec = &rc
	}
	c := pixapi.CobV{
		Calendario:         cal,
		TxID:               txid,
		Loc:                locSnapshot(l),
		Status:             pixapi.StatusAtiva,
		Devedor:            req.Devedor,
		Recebedor:          rec,
		Valor:              req.Valor,
		Chave:              req.Chave,
		SolicitacaoPagador: req.SolicitacaoPagador,
		InfoAdicionais:     req.InfoAdicionais,
		PixCopiaECola:      s.brcode(l),
	}
	s.cobvs[txid] = []pixapi.CobV{c}
	return &c, nil
}

func (s *Server) getCobV(r *http.Request) (int, any) {
	revs := s.cobvs[r.PathValue("txid")]
	if revs == nil {
		return cobvNotFound()
	}
	c := revs[len(revs)-1]
	return http.StatusOK, &c
}

func (s *Server) patchCobV(r *http.Request) (int, any) {
	var p pixapi.CobVPatch
	if err := decode(r, &p); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "Corpo da requisição inválido: "+err.Error())
	}
	c, prob := s.patchCobVLocked(r.PathValue("txid"), p)
	if prob != nil {
		return prob.Status, prob
	}
	return http.StatusOK, c
}

func (s *Server) patchCobVLocked(txid string, p pixapi.CobVPatch) (*pixapi.CobV, *pixapi.Problem) {
	revs := s.cobvs[txid]
	if revs == nil {
		_, v := cobvNotFound()
		return nil, v.(*pixapi.Problem)
	}
	if err := p.Validate(); err != nil {
		return nil, validationProblem(pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, err)
	}
	cur := revs[len(revs)-1]
	if cur.Status != pixapi.StatusAtiva {
		return nil, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "A cobrança não está ATIVA (status "+string(cur.Status)+").")
	}
	if p.Chave != "" && !s.ownsKey(p.Chave) {
		return nil, problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, detailChaveNotOwned, viol("chave", "não pertence ao recebedor"))
	}
	next := cur
	if p.Loc != nil && (cur.Loc == nil || p.Loc.ID != cur.Loc.ID) {
		l, prob := s.linkLocLocked(p.Loc, pixapi.TipoCobVencimento, txid, pixapi.TypeCobVOperacaoInvalida)
		if prob != nil {
			return nil, prob
		}
		if cur.Loc != nil {
			if old := s.locs[cur.Loc.ID]; old != nil {
				old.TxID = ""
			}
		}
		next.Loc, next.PixCopiaECola = locSnapshot(l), s.brcode(l)
	}
	if p.Calendario != nil {
		cal := *p.Calendario
		cal.Criacao = cur.Calendario.Criacao
		if cal.ValidadeAposVencimento == 0 {
			cal.ValidadeAposVencimento = cur.Calendario.ValidadeAposVencimento
		}
		next.Calendario = cal
	}
	if p.Devedor != nil {
		next.Devedor = p.Devedor
	}
	if p.Valor != nil {
		next.Valor = *p.Valor
	}
	if p.Chave != "" {
		next.Chave = p.Chave
	}
	if p.SolicitacaoPagador != nil {
		next.SolicitacaoPagador = *p.SolicitacaoPagador
	}
	if p.InfoAdicionais != nil {
		next.InfoAdicionais = p.InfoAdicionais
	}
	if p.Status != "" {
		next.Status = p.Status
	}
	next.Revisao = cur.Revisao + 1
	s.cobvs[txid] = append(revs, next)
	return &next, nil
}

func (s *Server) listCobV(r *http.Request) (int, any) {
	inicio, fim, p := timeRange(r, true)
	if p != nil {
		return p.Status, p
	}
	q := r.URL.Query()
	var items []pixapi.CobV
	for _, revs := range s.cobvs {
		c := revs[len(revs)-1]
		if !inRange(*c.Calendario.Criacao, inicio, fim) || !matchDoc(c.Devedor, q.Get("cpf"), q.Get("cnpj")) {
			continue
		}
		if st := q.Get("status"); st != "" && string(c.Status) != st {
			continue
		}
		if lp := q.Get("locationPresente"); lp != "" && (c.Loc != nil) != (lp == "true") {
			continue
		}
		items = append(items, c)
	}
	sortByTime(items, func(c pixapi.CobV) time.Time { return *c.Calendario.Criacao }, func(c pixapi.CobV) string { return c.TxID })
	page, params, p := paginate(r, items, inicio, fim)
	if p != nil {
		return p.Status, p
	}
	if page == nil {
		page = []pixapi.CobV{}
	}
	return http.StatusOK, map[string]any{"parametros": params, "cobs": page}
}

// ---- lotecobv ------------------------------------------------------------

func (s *Server) putLote(r *http.Request) (int, any) {
	id, p := pathID(r)
	if p != nil {
		return p.Status, p
	}
	if s.lotes[id] != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLoteCobVOperacaoInvalida, titleLoteInvalido, "Já existe um lote com o id informado.")
	}
	var req pixapi.LoteCobVRequest
	if err := decode(r, &req); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLoteCobVOperacaoInvalida, titleLoteInvalido, "Corpo da requisição inválido: "+err.Error())
	}
	if len(req.CobsV) == 0 {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLoteCobVOperacaoInvalida, titleLoteInvalido, "O lote não contém cobranças.", viol("cobsv", "vazio"))
	}
	now := s.now()
	lote := &pixapi.LoteCobV{ID: id, Descricao: req.Descricao, Criacao: &now}
	for _, it := range req.CobsV {
		lote.CobsV = append(lote.CobsV, s.loteItemLocked(it, now))
	}
	s.lotes[id] = lote
	return http.StatusAccepted, nil
}

// loteItemLocked creates one charge of a batch. The mock processes batches
// synchronously, so no item is left EM_PROCESSAMENTO.
func (s *Server) loteItemLocked(it pixapi.LoteCobVRequestItem, now time.Time) pixapi.LoteCobVItem {
	if err := pixapi.ValidateTxID(it.TxID); err != nil {
		return pixapi.LoteCobVItem{TxID: it.TxID, Status: pixapi.LoteNegada,
			Problema: problem(http.StatusBadRequest, pixapi.TypeCobVOperacaoInvalida, titleCobVInvalida, "txid inválido.", viol("txid", err.Error()))}
	}
	if _, p := s.createCobVLocked(it.TxID, it.CobVRequest); p != nil {
		return pixapi.LoteCobVItem{TxID: it.TxID, Status: pixapi.LoteNegada, Problema: p}
	}
	return pixapi.LoteCobVItem{TxID: it.TxID, Status: pixapi.LoteCriada, Criacao: &now}
}

func (s *Server) patchLote(r *http.Request) (int, any) {
	id, p := pathID(r)
	if p != nil {
		return p.Status, p
	}
	lote := s.lotes[id]
	if lote == nil {
		return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeLoteCobVNaoEncontrado, titleLoteNotFound, "Não existe lote para o id informado.")
	}
	var req pixapi.LoteCobVRequest
	if err := decode(r, &req); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLoteCobVOperacaoInvalida, titleLoteInvalido, "Corpo da requisição inválido: "+err.Error())
	}
	index := map[string]int{}
	for i, it := range lote.CobsV {
		index[it.TxID] = i
	}
	for _, it := range req.CobsV {
		if _, ok := index[it.TxID]; !ok {
			return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLoteCobVOperacaoInvalida, titleLoteInvalido,
				"O txid "+it.TxID+" não pertence ao lote.", viol("cobsv.txid", "não pertence ao lote"))
		}
	}
	now := s.now()
	for _, it := range req.CobsV {
		i := index[it.TxID]
		if lote.CobsV[i].Status == pixapi.LoteNegada {
			lote.CobsV[i] = s.loteItemLocked(it, now)
			continue
		}
		cal := it.Calendario
		patch := pixapi.CobVPatch{Calendario: &cal, Devedor: it.Devedor, Loc: it.Loc, Valor: &it.Valor, Chave: it.Chave,
			SolicitacaoPagador: &it.SolicitacaoPagador, InfoAdicionais: it.InfoAdicionais}
		if _, prob := s.patchCobVLocked(it.TxID, patch); prob != nil {
			lote.CobsV[i].Problema = prob
		} else {
			lote.CobsV[i].Problema = nil
		}
	}
	if req.Descricao != "" {
		lote.Descricao = req.Descricao
	}
	return http.StatusAccepted, nil
}

func (s *Server) getLote(r *http.Request) (int, any) {
	id, p := pathID(r)
	if p != nil {
		return p.Status, p
	}
	lote := s.lotes[id]
	if lote == nil {
		return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeLoteCobVNaoEncontrado, titleLoteNotFound, "Não existe lote para o id informado.")
	}
	return http.StatusOK, lote
}

func (s *Server) listLote(r *http.Request) (int, any) {
	inicio, fim, p := timeRange(r, true)
	if p != nil {
		return p.Status, p
	}
	var items []pixapi.LoteCobV
	for _, l := range s.lotes {
		if inRange(*l.Criacao, inicio, fim) {
			items = append(items, *l)
		}
	}
	sortByTime(items, func(l pixapi.LoteCobV) time.Time { return *l.Criacao }, func(l pixapi.LoteCobV) string { return fmt.Sprintf("%020d", l.ID) })
	page, params, p := paginate(r, items, inicio, fim)
	if p != nil {
		return p.Status, p
	}
	if page == nil {
		page = []pixapi.LoteCobV{}
	}
	return http.StatusOK, map[string]any{"parametros": params, "lotes": page}
}

// ---- loc -----------------------------------------------------------------

func locNotFound() (int, any) {
	return http.StatusNotFound, problem(http.StatusNotFound, pixapi.TypeLocNaoEncontrado, titleLocNotFound, "Não existe location para o id informado.")
}

func (s *Server) postLoc(r *http.Request) (int, any) {
	var req struct {
		TipoCob pixapi.TipoCob `json:"tipoCob"`
	}
	if err := decode(r, &req); err != nil {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLocOperacaoInvalida, titleLocInvalida, "Corpo da requisição inválido: "+err.Error())
	}
	if req.TipoCob != pixapi.TipoCobImediata && req.TipoCob != pixapi.TipoCobVencimento {
		return http.StatusBadRequest, problem(http.StatusBadRequest, pixapi.TypeLocOperacaoInvalida, titleLocInvalida, "tipoCob inválido.", viol("tipoCob", "deve ser cob ou cobv"))
	}
	l := *s.newLocLocked(req.TipoCob)
	return http.StatusCreated, &l
}

func (s *Server) getLoc(r *http.Request) (int, any) {
	id, p := pathID(r)
	if p != nil {
		return p.Status, p
	}
	l := s.locs[id]
	if l == nil {
		return locNotFound()
	}
	c := *l
	return http.StatusOK, &c
}

func (s *Server) unlinkLoc(r *http.Request) (int, any) {
	id, p := pathID(r)
	if p != nil {
		return p.Status, p
	}
	l := s.locs[id]
	if l == nil {
		return locNotFound()
	}
	if l.TxID != "" {
		if revs := s.cobs[l.TxID]; l.TipoCob == pixapi.TipoCobImediata && revs != nil {
			c := &revs[len(revs)-1]
			c.Loc, c.Location, c.PixCopiaECola = nil, "", ""
		}
		if revs := s.cobvs[l.TxID]; l.TipoCob == pixapi.TipoCobVencimento && revs != nil {
			c := &revs[len(revs)-1]
			c.Loc, c.PixCopiaECola = nil, ""
		}
		l.TxID = ""
	}
	c := *l
	return http.StatusOK, &c
}

func (s *Server) listLoc(r *http.Request) (int, any) {
	inicio, fim, p := timeRange(r, true)
	if p != nil {
		return p.Status, p
	}
	q := r.URL.Query()
	var items []pixapi.Loc
	for _, l := range s.locs {
		if !inRange(*l.Criacao, inicio, fim) {
			continue
		}
		if tp := q.Get("txIdPresente"); tp != "" && (l.TxID != "") != (tp == "true") {
			continue
		}
		if tc := q.Get("tipoCob"); tc != "" && string(l.TipoCob) != tc {
			continue
		}
		items = append(items, *l)
	}
	sortByTime(items, func(l pixapi.Loc) time.Time { return *l.Criacao }, func(l pixapi.Loc) string { return fmt.Sprintf("%020d", l.ID) })
	page, params, p := paginate(r, items, inicio, fim)
	if p != nil {
		return p.Status, p
	}
	if page == nil {
		page = []pixapi.Loc{}
	}
	return http.StatusOK, map[string]any{"parametros": params, "loc": page}
}
