package pixapi

import (
	"errors"
	"fmt"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/victorcondackdev/brpay/document"
	"github.com/victorcondackdev/brpay/pix"
)

// Limits checked locally before a request is sent. They mirror the
// constraints of the API Pix schema; verify against the current BCB spec
// version, as PSPs may enforce stricter ones.
const (
	MinTxIDLength               = 26
	MaxTxIDLength               = 35
	MaxDevolucaoIDLength        = 35
	MaxSolicitacaoPagadorLength = 140
	MaxInfoAdicionais           = 50
	MaxInfoAdicionalNomeLength  = 50
	MaxInfoAdicionalValorLength = 200
	MaxDevedorNomeLength        = 200
	MaxDescricaoDevolucaoLength = 140
)

// ValidateTxID reports whether txid can be used to create a charge with
// PUT: 26 to 35 characters from [a-zA-Z0-9].
func ValidateTxID(txid string) error {
	if n := len(txid); n < MinTxIDLength || n > MaxTxIDLength {
		return fmt.Errorf("txid must have %d to %d characters, got %d", MinTxIDLength, MaxTxIDLength, n)
	}
	if i := indexNonAlnum(txid); i >= 0 {
		return fmt.Errorf("txid must be alphanumeric, found %q at position %d", txid[i], i)
	}
	return nil
}

// validateTxIDRef checks a txid used to address an existing charge. It is
// more lenient on length (1 to 35) because PSPs generate txids for POST
// /cob and static codes may carry shorter ones.
func validateTxIDRef(txid string) error {
	if n := len(txid); n < 1 || n > MaxTxIDLength {
		return fmt.Errorf("txid must have 1 to %d characters, got %d", MaxTxIDLength, n)
	}
	if i := indexNonAlnum(txid); i >= 0 {
		return fmt.Errorf("txid must be alphanumeric, found %q at position %d", txid[i], i)
	}
	return nil
}

// ValidateDevolucaoID reports whether id is a valid refund id: 1 to 35
// characters from [a-zA-Z0-9].
func ValidateDevolucaoID(id string) error {
	if n := len(id); n < 1 || n > MaxDevolucaoIDLength {
		return fmt.Errorf("devolucao id must have 1 to %d characters, got %d", MaxDevolucaoIDLength, n)
	}
	if i := indexNonAlnum(id); i >= 0 {
		return fmt.Errorf("devolucao id must be alphanumeric, found %q at position %d", id[i], i)
	}
	return nil
}

// ValidateEndToEndID reports whether e2eid is a well-formed Pix
// EndToEndId, using brpay's parser (which also checks the timestamp).
func ValidateEndToEndID(e2eid string) error {
	_, err := pix.ParseEndToEndID(e2eid)
	return err
}

// ValidateDecimal reports whether s is a two-decimal API Pix number such as
// "2.00", as used by valorPerc.
func ValidateDecimal(s string) error {
	_, err := ParseAmount(s)
	return err
}

func indexNonAlnum(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			return i
		}
	}
	return -1
}

// validator accumulates violations.
type validator struct{ v []Violacao }

func (v *validator) add(prop, reason string, args ...any) {
	v.v = append(v.v, Violacao{Propriedade: prop, Razao: fmt.Sprintf(reason, args...)})
}

func (v *validator) err() error {
	if len(v.v) == 0 {
		return nil
	}
	return &ValidationError{Violacoes: v.v}
}

func (v *validator) maxLen(prop, s string, n int) {
	if c := utf8.RuneCountInString(s); c > n {
		v.add(prop, "must have at most %d characters, got %d", n, c)
	}
}

// chave validates and normalizes a Pix key in place.
func (v *validator) chave(prop string, k *string) {
	if *k == "" {
		v.add(prop, "is required")
		return
	}
	norm, _, err := pix.NormalizeKey(*k)
	if err != nil {
		v.add(prop, "is not a valid Pix key")
		return
	}
	*k = norm
}

// devedor validates and normalizes a payer. When required is set, a
// document and a name must be present.
func (v *validator) devedor(d *Devedor, required bool) {
	if d == nil {
		if required {
			v.add("devedor", "is required")
		}
		return
	}
	switch {
	case d.CPF != "" && d.CNPJ != "":
		v.add("devedor", "cpf and cnpj are mutually exclusive")
	case d.CPF != "":
		if err := document.ValidateCPF(d.CPF); err != nil {
			v.add("devedor.cpf", "is not a valid CPF")
		} else {
			d.CPF = document.Clean(d.CPF)
		}
	case d.CNPJ != "":
		if err := document.ValidateCNPJ(d.CNPJ); err != nil {
			v.add("devedor.cnpj", "is not a valid CNPJ")
		} else {
			d.CNPJ = document.Clean(d.CNPJ)
		}
	case required:
		v.add("devedor", "cpf or cnpj is required")
	}
	if (d.CPF != "" || d.CNPJ != "" || required) && d.Nome == "" {
		v.add("devedor.nome", "is required when a document is present")
	}
	v.maxLen("devedor.nome", d.Nome, MaxDevedorNomeLength)
	if d.UF != "" && len(d.UF) != 2 {
		v.add("devedor.uf", "must have 2 letters")
	}
	if d.CEP != "" {
		if c := document.Clean(d.CEP); len(c) != 8 || indexNonDigit(c) >= 0 {
			v.add("devedor.cep", "must have 8 digits")
		}
	}
}

func indexNonDigit(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return i
		}
	}
	return -1
}

func (v *validator) common(solicitacao string, infos []InfoAdicional) {
	v.maxLen("solicitacaoPagador", solicitacao, MaxSolicitacaoPagadorLength)
	if len(infos) > MaxInfoAdicionais {
		v.add("infoAdicionais", "must have at most %d entries, got %d", MaxInfoAdicionais, len(infos))
	}
	for i, ia := range infos {
		p := fmt.Sprintf("infoAdicionais[%d]", i)
		if ia.Nome == "" {
			v.add(p+".nome", "is required")
		}
		if ia.Valor == "" {
			v.add(p+".valor", "is required")
		}
		v.maxLen(p+".nome", ia.Nome, MaxInfoAdicionalNomeLength)
		v.maxLen(p+".valor", ia.Valor, MaxInfoAdicionalValorLength)
	}
}

func (v *validator) valor(prop string, val Valor) {
	if !val.Original.Valid() {
		v.add(prop+".original", "is out of range")
	}
	switch val.ModalidadeAlteracao {
	case 0:
		if val.Original <= 0 {
			v.add(prop+".original", "must be greater than zero")
		}
	case 1:
	default:
		v.add(prop+".modalidadeAlteracao", "must be 0 or 1")
	}
}

func (v *validator) calendario(c *Calendario) {
	if c == nil {
		return
	}
	if c.Expiracao < 0 {
		v.add("calendario.expiracao", "must be a positive number of seconds")
	}
}

// Validate checks the request and normalizes the key and documents in
// place. It is called by the client before sending.
func (r *CobRequest) Validate() error {
	var v validator
	v.calendario(r.Calendario)
	v.devedor(r.Devedor, false)
	v.valor("valor", r.Valor)
	v.chave("chave", &r.Chave)
	v.common(r.SolicitacaoPagador, r.InfoAdicionais)
	return v.err()
}

// Validate checks the patch and normalizes the key and documents in place.
func (p *CobPatch) Validate() error {
	var v validator
	v.calendario(p.Calendario)
	v.devedor(p.Devedor, false)
	if p.Valor != nil {
		v.valor("valor", *p.Valor)
	}
	if p.Chave != "" {
		v.chave("chave", &p.Chave)
	}
	if p.Status != "" && p.Status != StatusRemovidaPeloUsuarioRecebedor {
		v.add("status", "only %s can be set by the receiver", StatusRemovidaPeloUsuarioRecebedor)
	}
	sol := ""
	if p.SolicitacaoPagador != nil {
		sol = *p.SolicitacaoPagador
	}
	v.common(sol, p.InfoAdicionais)
	return v.err()
}

func (v *validator) calendarioCobV(c CalendarioCobV) {
	if c.DataDeVencimento == "" {
		v.add("calendario.dataDeVencimento", "is required")
	} else if _, err := time.Parse(time.DateOnly, c.DataDeVencimento); err != nil {
		v.add("calendario.dataDeVencimento", "must be a date formatted YYYY-MM-DD")
	}
	if c.ValidadeAposVencimento < 0 {
		v.add("calendario.validadeAposVencimento", "must not be negative")
	}
}

func (v *validator) modalidade(prop string, m *Modalidade) {
	if m == nil {
		return
	}
	if m.Modalidade < 1 {
		v.add(prop+".modalidade", "must be a positive code")
	}
	if ValidateDecimal(m.ValorPerc) != nil {
		v.add(prop+".valorPerc", "must be a decimal with two places")
	}
}

func (v *validator) valorCobV(val ValorCobV) {
	if !val.Original.Valid() || val.Original <= 0 {
		v.add("valor.original", "must be greater than zero")
	}
	v.modalidade("valor.multa", val.Multa)
	v.modalidade("valor.juros", val.Juros)
	v.modalidade("valor.abatimento", val.Abatimento)
	if d := val.Desconto; d != nil {
		if d.Modalidade < 1 {
			v.add("valor.desconto.modalidade", "must be a positive code")
		}
		if d.ValorPerc != "" && ValidateDecimal(d.ValorPerc) != nil {
			v.add("valor.desconto.valorPerc", "must be a decimal with two places")
		}
		if d.ValorPerc == "" && len(d.DescontoDataFixa) == 0 {
			v.add("valor.desconto", "valorPerc or descontoDataFixa is required")
		}
		for i, f := range d.DescontoDataFixa {
			p := fmt.Sprintf("valor.desconto.descontoDataFixa[%d]", i)
			if _, err := time.Parse(time.DateOnly, f.Data); err != nil {
				v.add(p+".data", "must be a date formatted YYYY-MM-DD")
			}
			if ValidateDecimal(f.ValorPerc) != nil {
				v.add(p+".valorPerc", "must be a decimal with two places")
			}
		}
	}
}

// Validate checks the request and normalizes the key and documents in
// place.
func (r *CobVRequest) Validate() error {
	var v validator
	v.calendarioCobV(r.Calendario)
	v.devedor(r.Devedor, true)
	v.valorCobV(r.Valor)
	v.chave("chave", &r.Chave)
	v.common(r.SolicitacaoPagador, r.InfoAdicionais)
	return v.err()
}

// Validate checks the patch and normalizes the key and documents in place.
func (p *CobVPatch) Validate() error {
	var v validator
	if p.Calendario != nil {
		v.calendarioCobV(*p.Calendario)
	}
	v.devedor(p.Devedor, false)
	if p.Valor != nil {
		v.valorCobV(*p.Valor)
	}
	if p.Chave != "" {
		v.chave("chave", &p.Chave)
	}
	if p.Status != "" && p.Status != StatusRemovidaPeloUsuarioRecebedor {
		v.add("status", "only %s can be set by the receiver", StatusRemovidaPeloUsuarioRecebedor)
	}
	sol := ""
	if p.SolicitacaoPagador != nil {
		sol = *p.SolicitacaoPagador
	}
	v.common(sol, p.InfoAdicionais)
	return v.err()
}

// Validate checks the refund request.
func (r *DevolucaoRequest) Validate() error {
	var v validator
	if r.Valor <= 0 || !r.Valor.Valid() {
		v.add("valor", "must be greater than zero")
	}
	// Natureza is not checked: later spec revisions added values, and the
	// PSP is the authority on which ones it supports.
	v.maxLen("descricao", r.Descricao, MaxDescricaoDevolucaoLength)
	return v.err()
}

// Validate checks the batch and each of its charges.
func (r *LoteCobVRequest) Validate() error {
	var v validator
	if len(r.CobsV) == 0 {
		v.add("cobsv", "must not be empty")
	}
	seen := make(map[string]bool, len(r.CobsV))
	for i := range r.CobsV {
		item := &r.CobsV[i]
		p := fmt.Sprintf("cobsv[%d]", i)
		if err := ValidateTxID(item.TxID); err != nil {
			v.add(p+".txid", "%s", err.Error())
		} else if seen[item.TxID] {
			v.add(p+".txid", "is duplicated in the batch")
		}
		seen[item.TxID] = true
		if err := item.CobVRequest.Validate(); err != nil {
			for _, vi := range err.(*ValidationError).Violacoes {
				v.add(p+"."+vi.Propriedade, "%s", vi.Razao)
			}
		}
	}
	return v.err()
}

// validateWebhookURL requires an absolute https URL.
func validateWebhookURL(raw string) error {
	var v validator
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		v.add("webhookUrl", "must be an absolute URL")
	} else if u.Scheme != "https" {
		v.add("webhookUrl", "must use https")
	}
	return v.err()
}

func invalidParam(prop string, err error) error {
	return &ValidationError{Violacoes: []Violacao{{Propriedade: prop, Razao: err.Error()}}}
}

var errNegativeID = errors.New("must not be negative")
