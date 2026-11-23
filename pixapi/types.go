package pixapi

import "time"

// Status is the lifecycle status of a charge (cob or cobv).
type Status string

// Charge statuses defined by the API Pix.
const (
	StatusAtiva                        Status = "ATIVA"
	StatusConcluida                    Status = "CONCLUIDA"
	StatusRemovidaPeloUsuarioRecebedor Status = "REMOVIDA_PELO_USUARIO_RECEBEDOR"
	StatusRemovidaPeloPSP              Status = "REMOVIDA_PELO_PSP"
)

// Final reports whether no further transition is possible from s.
func (s Status) Final() bool { return s != StatusAtiva && s != "" }

// TipoCob identifies the kind of charge a payload location serves.
type TipoCob string

// Charge kinds accepted by POST /loc.
const (
	TipoCobImediata   TipoCob = "cob"
	TipoCobVencimento TipoCob = "cobv"
)

// DefaultExpiracao is the expiry, in seconds, a PSP applies to an immediate
// charge when calendario.expiracao is omitted (24 hours).
const DefaultExpiracao = 86400

// DefaultValidadeAposVencimento is the number of days a charge with due date
// remains payable after dataDeVencimento when the field is omitted.
const DefaultValidadeAposVencimento = 30

// Calendario holds the time attributes of an immediate charge.
type Calendario struct {
	// Criacao is set by the PSP.
	Criacao *time.Time `json:"criacao,omitempty"`
	// Expiracao is the lifetime of the charge in seconds, counted from
	// Criacao. Zero means DefaultExpiracao.
	Expiracao int `json:"expiracao,omitempty"`
}

// CalendarioCobV holds the time attributes of a charge with due date.
type CalendarioCobV struct {
	// Criacao is set by the PSP.
	Criacao *time.Time `json:"criacao,omitempty"`
	// DataDeVencimento is the due date, formatted YYYY-MM-DD.
	DataDeVencimento string `json:"dataDeVencimento"`
	// ValidadeAposVencimento is the number of days the charge can still be
	// paid after the due date. Zero means DefaultValidadeAposVencimento.
	ValidadeAposVencimento int `json:"validadeAposVencimento,omitempty"`
}

// Devedor identifies the payer of a charge. CPF and CNPJ are mutually
// exclusive; when either is present, Nome is required. The address and
// e-mail fields are only used by charges with due date.
type Devedor struct {
	CPF        string `json:"cpf,omitempty"`
	CNPJ       string `json:"cnpj,omitempty"`
	Nome       string `json:"nome,omitempty"`
	Email      string `json:"email,omitempty"`
	Logradouro string `json:"logradouro,omitempty"`
	Cidade     string `json:"cidade,omitempty"`
	UF         string `json:"uf,omitempty"`
	CEP        string `json:"cep,omitempty"`
}

// Recebedor identifies the receiver of a charge with due date, as filled in
// by the PSP.
type Recebedor struct {
	CNPJ         string `json:"cnpj,omitempty"`
	CPF          string `json:"cpf,omitempty"`
	Nome         string `json:"nome,omitempty"`
	NomeFantasia string `json:"nomeFantasia,omitempty"`
	Logradouro   string `json:"logradouro,omitempty"`
	Cidade       string `json:"cidade,omitempty"`
	UF           string `json:"uf,omitempty"`
	CEP          string `json:"cep,omitempty"`
}

// Valor is the amount of an immediate charge.
type Valor struct {
	Original Amount `json:"original"`
	// ModalidadeAlteracao 1 lets the payer change the amount; 0 (the
	// default) does not.
	ModalidadeAlteracao int `json:"modalidadeAlteracao,omitempty"`
}

// Modalidade is a fine, interest or rebate rule of a charge with due date:
// Modalidade selects how ValorPerc is interpreted (fixed value or
// percentage, per day, month or year, calendar or business days).
//
// The numeric codes and their meaning are defined by the API Pix
// specification and are not interpreted by this package: verify them
// against the current BCB spec version.
type Modalidade struct {
	Modalidade int `json:"modalidade"`
	// ValorPerc is a decimal string with two places, a value in reais or a
	// percentage depending on Modalidade.
	ValorPerc string `json:"valorPerc"`
}

// DescontoDataFixa is a discount granted until a fixed date.
type DescontoDataFixa struct {
	Data      string `json:"data"` // YYYY-MM-DD
	ValorPerc string `json:"valorPerc"`
}

// Desconto is the discount rule of a charge with due date. Fixed-date
// modalities use DescontoDataFixa; the others use ValorPerc.
type Desconto struct {
	Modalidade       int                `json:"modalidade"`
	ValorPerc        string             `json:"valorPerc,omitempty"`
	DescontoDataFixa []DescontoDataFixa `json:"descontoDataFixa,omitempty"`
}

// ValorCobV is the amount of a charge with due date and the rules a PSP
// applies to compute the final amount at payment time.
type ValorCobV struct {
	Original   Amount      `json:"original"`
	Multa      *Modalidade `json:"multa,omitempty"`
	Juros      *Modalidade `json:"juros,omitempty"`
	Abatimento *Modalidade `json:"abatimento,omitempty"`
	Desconto   *Desconto   `json:"desconto,omitempty"`
}

// InfoAdicional is a name/value pair shown to the payer.
type InfoAdicional struct {
	Nome  string `json:"nome"`
	Valor string `json:"valor"`
}

// LocRef references an existing payload location by id when creating a
// charge.
type LocRef struct {
	ID int64 `json:"id"`
}

// Loc is a payload location: the URL, stored in a dynamic BR Code, from
// which the payer's app fetches the charge.
type Loc struct {
	ID       int64      `json:"id"`
	Location string     `json:"location"`
	TipoCob  TipoCob    `json:"tipoCob"`
	Criacao  *time.Time `json:"criacao,omitempty"`
	// TxID is the charge currently linked to the location, if any.
	TxID string `json:"txid,omitempty"`
}

// CobRequest is the body of PUT /cob/{txid} and POST /cob.
type CobRequest struct {
	Calendario         *Calendario     `json:"calendario,omitempty"`
	Devedor            *Devedor        `json:"devedor,omitempty"`
	Loc                *LocRef         `json:"loc,omitempty"`
	Valor              Valor           `json:"valor"`
	Chave              string          `json:"chave"`
	SolicitacaoPagador string          `json:"solicitacaoPagador,omitempty"`
	InfoAdicionais     []InfoAdicional `json:"infoAdicionais,omitempty"`
}

// CobPatch is the body of PATCH /cob/{txid}. Nil fields are left unchanged.
// Setting Status to StatusRemovidaPeloUsuarioRecebedor cancels the charge.
type CobPatch struct {
	Calendario         *Calendario     `json:"calendario,omitempty"`
	Devedor            *Devedor        `json:"devedor,omitempty"`
	Loc                *LocRef         `json:"loc,omitempty"`
	Status             Status          `json:"status,omitempty"`
	Valor              *Valor          `json:"valor,omitempty"`
	Chave              string          `json:"chave,omitempty"`
	SolicitacaoPagador *string         `json:"solicitacaoPagador,omitempty"`
	InfoAdicionais     []InfoAdicional `json:"infoAdicionais,omitempty"`
}

// Cob is an immediate charge as returned by the PSP.
type Cob struct {
	Calendario         Calendario      `json:"calendario"`
	TxID               string          `json:"txid"`
	Revisao            int             `json:"revisao"`
	Loc                *Loc            `json:"loc,omitempty"`
	Location           string          `json:"location,omitempty"`
	Status             Status          `json:"status"`
	Devedor            *Devedor        `json:"devedor,omitempty"`
	Valor              Valor           `json:"valor"`
	Chave              string          `json:"chave"`
	SolicitacaoPagador string          `json:"solicitacaoPagador,omitempty"`
	InfoAdicionais     []InfoAdicional `json:"infoAdicionais,omitempty"`
	PixCopiaECola      string          `json:"pixCopiaECola,omitempty"`
	// Pix lists the payments received for the charge.
	Pix []Pix `json:"pix,omitempty"`
}

// CobVRequest is the body of PUT /cobv/{txid}. Devedor, with Nome, is
// required for charges with due date.
type CobVRequest struct {
	Calendario         CalendarioCobV  `json:"calendario"`
	Devedor            *Devedor        `json:"devedor,omitempty"`
	Loc                *LocRef         `json:"loc,omitempty"`
	Valor              ValorCobV       `json:"valor"`
	Chave              string          `json:"chave"`
	SolicitacaoPagador string          `json:"solicitacaoPagador,omitempty"`
	InfoAdicionais     []InfoAdicional `json:"infoAdicionais,omitempty"`
}

// CobVPatch is the body of PATCH /cobv/{txid}. Nil fields are left
// unchanged.
type CobVPatch struct {
	Calendario         *CalendarioCobV `json:"calendario,omitempty"`
	Devedor            *Devedor        `json:"devedor,omitempty"`
	Loc                *LocRef         `json:"loc,omitempty"`
	Status             Status          `json:"status,omitempty"`
	Valor              *ValorCobV      `json:"valor,omitempty"`
	Chave              string          `json:"chave,omitempty"`
	SolicitacaoPagador *string         `json:"solicitacaoPagador,omitempty"`
	InfoAdicionais     []InfoAdicional `json:"infoAdicionais,omitempty"`
}

// CobV is a charge with due date as returned by the PSP.
type CobV struct {
	Calendario         CalendarioCobV  `json:"calendario"`
	TxID               string          `json:"txid"`
	Revisao            int             `json:"revisao"`
	Loc                *Loc            `json:"loc,omitempty"`
	Status             Status          `json:"status"`
	Devedor            *Devedor        `json:"devedor,omitempty"`
	Recebedor          *Recebedor      `json:"recebedor,omitempty"`
	Valor              ValorCobV       `json:"valor"`
	Chave              string          `json:"chave"`
	SolicitacaoPagador string          `json:"solicitacaoPagador,omitempty"`
	InfoAdicionais     []InfoAdicional `json:"infoAdicionais,omitempty"`
	PixCopiaECola      string          `json:"pixCopiaECola,omitempty"`
	Pix                []Pix           `json:"pix,omitempty"`
}

// Pix is a payment received by the merchant.
type Pix struct {
	EndToEndID string `json:"endToEndId"`
	// TxID is the charge the payment settled, if any.
	TxID        string      `json:"txid,omitempty"`
	Valor       Amount      `json:"valor"`
	Chave       string      `json:"chave,omitempty"`
	Horario     time.Time   `json:"horario"`
	InfoPagador string      `json:"infoPagador,omitempty"`
	Devolucoes  []Devolucao `json:"devolucoes,omitempty"`
}

// Refunded returns the sum of the devoluções that have not failed.
func (p Pix) Refunded() Amount {
	var total Amount
	for _, d := range p.Devolucoes {
		if d.Status != DevolucaoNaoRealizado {
			total += d.Valor
		}
	}
	return total
}

// DevolucaoStatus is the status of a refund.
type DevolucaoStatus string

// Refund statuses.
const (
	DevolucaoEmProcessamento DevolucaoStatus = "EM_PROCESSAMENTO"
	DevolucaoDevolvido       DevolucaoStatus = "DEVOLVIDO"
	DevolucaoNaoRealizado    DevolucaoStatus = "NAO_REALIZADO"
)

// Natureza is the nature of a refund. Later revisions of the specification
// added values beyond these two; verify against the current BCB spec
// version before relying on them.
type Natureza string

// Refund natures.
const (
	NaturezaOriginal Natureza = "ORIGINAL"
	NaturezaRetirada Natureza = "RETIRADA"
)

// DevolucaoRequest is the body of PUT /pix/{e2eid}/devolucao/{id}.
type DevolucaoRequest struct {
	Valor     Amount   `json:"valor"`
	Natureza  Natureza `json:"natureza,omitempty"`
	Descricao string   `json:"descricao,omitempty"`
}

// DevolucaoHorario holds the timestamps of a refund.
type DevolucaoHorario struct {
	Solicitacao *time.Time `json:"solicitacao,omitempty"`
	Liquidacao  *time.Time `json:"liquidacao,omitempty"`
}

// Devolucao is a refund of a received Pix.
type Devolucao struct {
	ID        string           `json:"id"`
	RtrID     string           `json:"rtrId"`
	Valor     Amount           `json:"valor"`
	Natureza  Natureza         `json:"natureza,omitempty"`
	Descricao string           `json:"descricao,omitempty"`
	Horario   DevolucaoHorario `json:"horario"`
	Status    DevolucaoStatus  `json:"status"`
	Motivo    string           `json:"motivo,omitempty"`
}

// Webhook is a webhook registration.
type Webhook struct {
	WebhookURL string     `json:"webhookUrl"`
	Chave      string     `json:"chave"`
	Criacao    *time.Time `json:"criacao,omitempty"`
}

// LoteCobVRequest is the body of PUT and PATCH /lotecobv/{id}. Each entry
// carries its own txid.
type LoteCobVRequest struct {
	Descricao string                `json:"descricao,omitempty"`
	CobsV     []LoteCobVRequestItem `json:"cobsv"`
}

// LoteCobVRequestItem is a charge with due date inside a batch request.
type LoteCobVRequestItem struct {
	CobVRequest
	TxID string `json:"txid"`
}

// LoteItemStatus is the processing status of a charge inside a batch.
type LoteItemStatus string

// Batch item statuses.
const (
	LoteEmProcessamento LoteItemStatus = "EM_PROCESSAMENTO"
	LoteCriada          LoteItemStatus = "CRIADA"
	LoteNegada          LoteItemStatus = "NEGADA"
)

// LoteCobVItem is the status of one charge inside a batch.
type LoteCobVItem struct {
	TxID     string         `json:"txid"`
	Criacao  *time.Time     `json:"criacao,omitempty"`
	Status   LoteItemStatus `json:"status"`
	Problema *Problem       `json:"problema,omitempty"`
}

// LoteCobV is a batch of charges with due date.
type LoteCobV struct {
	ID        int64          `json:"id,omitempty"`
	Descricao string         `json:"descricao,omitempty"`
	Criacao   *time.Time     `json:"criacao,omitempty"`
	CobsV     []LoteCobVItem `json:"cobsv"`
}

// Paginacao describes the page returned by a list endpoint.
type Paginacao struct {
	PaginaAtual            int `json:"paginaAtual"`
	ItensPorPagina         int `json:"itensPorPagina"`
	QuantidadeDePaginas    int `json:"quantidadeDePaginas"`
	QuantidadeTotalDeItens int `json:"quantidadeTotalDeItens"`
}

// Parametros echoes the query of a list endpoint.
type Parametros struct {
	Inicio    time.Time `json:"inicio"`
	Fim       time.Time `json:"fim"`
	Paginacao Paginacao `json:"paginacao"`
}
