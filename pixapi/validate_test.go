package pixapi_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

func TestValidateTxID(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{strings.Repeat("a", 26), true},
		{strings.Repeat("Z", 35), true},
		{"abcDEF0123456789abcDEF0123", true},
		{strings.Repeat("a", 25), false},
		{strings.Repeat("a", 36), false},
		{"", false},
		{strings.Repeat("a", 25) + "-", false},
		{strings.Repeat("a", 25) + "é", false},
		{strings.Repeat("a", 25) + " ", false},
	}
	for _, tt := range tests {
		if err := pixapi.ValidateTxID(tt.in); (err == nil) != tt.ok {
			t.Errorf("ValidateTxID(%q) = %v, want ok=%v", tt.in, err, tt.ok)
		}
	}
}

func FuzzValidateTxID(f *testing.F) {
	for _, s := range []string{"", strings.Repeat("a", 26), strings.Repeat("a", 35), strings.Repeat("a", 36), "abc-def", "ção"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		err := pixapi.ValidateTxID(s)
		want := len(s) >= 26 && len(s) <= 35
		for _, c := range []byte(s) {
			if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
				want = false
			}
		}
		if want != (err == nil) {
			t.Fatalf("ValidateTxID(%q) = %v, want ok=%v", s, err, want)
		}
	})
}

func TestValidateIDs(t *testing.T) {
	if pixapi.ValidateDevolucaoID("D1") != nil || pixapi.ValidateDevolucaoID("") == nil ||
		pixapi.ValidateDevolucaoID(strings.Repeat("a", 36)) == nil || pixapi.ValidateDevolucaoID("a-b") == nil {
		t.Error("ValidateDevolucaoID")
	}
	if pixapi.ValidateEndToEndID("E12345678202611221530aB3dE5fG7hJ") != nil {
		t.Error("valid EndToEndId rejected")
	}
	if pixapi.ValidateEndToEndID("E1234") == nil {
		t.Error("short EndToEndId accepted")
	}
	if pixapi.ValidateDecimal("2.00") != nil || pixapi.ValidateDecimal("2") == nil {
		t.Error("ValidateDecimal")
	}
}

func props(err error) []string {
	var ve *pixapi.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out []string
	for _, v := range ve.Violacoes {
		out = append(out, v.Propriedade)
	}
	return out
}

func validCob() pixapi.CobRequest {
	return pixapi.CobRequest{
		Calendario: &pixapi.Calendario{Expiracao: 3600},
		Devedor:    &pixapi.Devedor{CPF: "123.456.789-09", Nome: "Fulano"},
		Valor:      pixapi.Valor{Original: 100},
		Chave:      "Fulano@Example.COM",
	}
}

func TestCobRequestValidate(t *testing.T) {
	r := validCob()
	if err := r.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	if r.Chave != "fulano@example.com" || r.Devedor.CPF != "12345678909" {
		t.Errorf("not normalized: chave %q cpf %q", r.Chave, r.Devedor.CPF)
	}
	long := strings.Repeat("x", 141)
	tests := []struct {
		name string
		mut  func(*pixapi.CobRequest)
		want string
	}{
		{"bad cpf", func(r *pixapi.CobRequest) { r.Devedor.CPF = "12345678900" }, "devedor.cpf"},
		{"bad cnpj", func(r *pixapi.CobRequest) { r.Devedor = &pixapi.Devedor{CNPJ: "12345678000100", Nome: "X"} }, "devedor.cnpj"},
		{"both docs", func(r *pixapi.CobRequest) { r.Devedor.CNPJ = "12345678000195" }, "devedor"},
		{"no name", func(r *pixapi.CobRequest) { r.Devedor.Nome = "" }, "devedor.nome"},
		{"bad key", func(r *pixapi.CobRequest) { r.Chave = "not a key" }, "chave"},
		{"no key", func(r *pixapi.CobRequest) { r.Chave = "" }, "chave"},
		{"zero amount", func(r *pixapi.CobRequest) { r.Valor.Original = 0 }, "valor.original"},
		{"huge amount", func(r *pixapi.CobRequest) { r.Valor.Original = pixapi.MaxAmount + 1 }, "valor.original"},
		{"bad modalidade", func(r *pixapi.CobRequest) { r.Valor.ModalidadeAlteracao = 2 }, "valor.modalidadeAlteracao"},
		{"negative expiry", func(r *pixapi.CobRequest) { r.Calendario.Expiracao = -1 }, "calendario.expiracao"},
		{"long solicitacao", func(r *pixapi.CobRequest) { r.SolicitacaoPagador = long }, "solicitacaoPagador"},
		{"info nome", func(r *pixapi.CobRequest) { r.InfoAdicionais = []pixapi.InfoAdicional{{Valor: "v"}} }, "infoAdicionais[0].nome"},
		{"info valor", func(r *pixapi.CobRequest) { r.InfoAdicionais = []pixapi.InfoAdicional{{Nome: "n"}} }, "infoAdicionais[0].valor"},
		{"too many infos", func(r *pixapi.CobRequest) {
			r.InfoAdicionais = make([]pixapi.InfoAdicional, 51)
			for i := range r.InfoAdicionais {
				r.InfoAdicionais[i] = pixapi.InfoAdicional{Nome: "n", Valor: "v"}
			}
		}, "infoAdicionais"},
		{"bad uf", func(r *pixapi.CobRequest) { r.Devedor.UF = "RJX" }, "devedor.uf"},
		{"bad cep", func(r *pixapi.CobRequest) { r.Devedor.CEP = "2000-000" }, "devedor.cep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validCob()
			d := *r.Devedor
			r.Devedor = &d
			tt.mut(&r)
			err := r.Validate()
			if !errors.Is(err, pixapi.ErrInvalidRequest) {
				t.Fatalf("Validate() = %v, want ErrInvalidRequest", err)
			}
			got := props(err)
			found := false
			for _, p := range got {
				found = found || p == tt.want
			}
			if !found {
				t.Errorf("violations %v, want %q", got, tt.want)
			}
		})
	}

	// Payer-defined amount allows zero.
	r = validCob()
	r.Valor = pixapi.Valor{Original: 0, ModalidadeAlteracao: 1}
	if err := r.Validate(); err != nil {
		t.Errorf("modalidadeAlteracao=1 with zero amount: %v", err)
	}
	r.Devedor = &pixapi.Devedor{CNPJ: "12.345.678/0001-95", Nome: "Empresa", CEP: "20000-000", UF: "RJ"}
	if err := r.Validate(); err != nil || r.Devedor.CNPJ != "12345678000195" {
		t.Errorf("CNPJ payer: %v %q", err, r.Devedor.CNPJ)
	}
}

func TestCobPatchValidate(t *testing.T) {
	ok := pixapi.CobPatch{Status: pixapi.StatusRemovidaPeloUsuarioRecebedor}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	sol := strings.Repeat("y", 200)
	bad := pixapi.CobPatch{Status: pixapi.StatusConcluida, Valor: &pixapi.Valor{}, Chave: "x", SolicitacaoPagador: &sol}
	got := props(bad.Validate())
	if len(got) != 4 {
		t.Errorf("violations = %v, want status, valor.original, chave, solicitacaoPagador", got)
	}
}

func validCobV() pixapi.CobVRequest {
	return pixapi.CobVRequest{
		Calendario: pixapi.CalendarioCobV{DataDeVencimento: "2026-12-31"},
		Devedor:    &pixapi.Devedor{CNPJ: "12345678000195", Nome: "Empresa"},
		Valor: pixapi.ValorCobV{
			Original: 1000,
			Multa:    &pixapi.Modalidade{Modalidade: 2, ValorPerc: "2.00"},
			Desconto: &pixapi.Desconto{Modalidade: 1, DescontoDataFixa: []pixapi.DescontoDataFixa{{Data: "2026-12-20", ValorPerc: "5.00"}}},
		},
		Chave: "+55 (21) 98765-4321",
	}
}

func TestCobVRequestValidate(t *testing.T) {
	r := validCobV()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Chave != "+5521987654321" {
		t.Errorf("phone key not normalized: %q", r.Chave)
	}
	r = validCobV()
	r.Devedor = nil
	r.Calendario = pixapi.CalendarioCobV{DataDeVencimento: "31/12/2026", ValidadeAposVencimento: -1}
	r.Valor.Juros = &pixapi.Modalidade{Modalidade: 0, ValorPerc: "1%"}
	r.Valor.Desconto = &pixapi.Desconto{Modalidade: 0, ValorPerc: "x", DescontoDataFixa: []pixapi.DescontoDataFixa{{Data: "nope", ValorPerc: "1"}}}
	want := []string{"calendario.dataDeVencimento", "calendario.validadeAposVencimento", "devedor", "valor.juros.modalidade", "valor.juros.valorPerc",
		"valor.desconto.modalidade", "valor.desconto.valorPerc", "valor.desconto.descontoDataFixa[0].data", "valor.desconto.descontoDataFixa[0].valorPerc"}
	got := props(r.Validate())
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("violations\n got %v\nwant %v", got, want)
	}
	r = validCobV()
	r.Calendario.DataDeVencimento = ""
	r.Devedor = &pixapi.Devedor{Nome: "X"}
	r.Valor.Desconto = &pixapi.Desconto{Modalidade: 3}
	got = props(r.Validate())
	if fmt.Sprint(got) != "[calendario.dataDeVencimento devedor valor.desconto]" {
		t.Errorf("violations = %v", got)
	}

	p := pixapi.CobVPatch{Calendario: &pixapi.CalendarioCobV{}, Valor: &pixapi.ValorCobV{}, Chave: "bad", Status: pixapi.StatusAtiva, Devedor: &pixapi.Devedor{CPF: "1"}}
	if got := props(p.Validate()); len(got) != 6 {
		t.Errorf("CobVPatch violations = %v", got)
	}
	sol := "ok"
	p = pixapi.CobVPatch{SolicitacaoPagador: &sol}
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
}

func TestDevolucaoAndLoteValidate(t *testing.T) {
	d := pixapi.DevolucaoRequest{Valor: 100, Natureza: pixapi.NaturezaOriginal}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	d = pixapi.DevolucaoRequest{Valor: 0, Descricao: strings.Repeat("d", 141)}
	if got := props(d.Validate()); len(got) != 2 {
		t.Errorf("violations = %v", got)
	}

	tx := strings.Repeat("a", 26)
	l := pixapi.LoteCobVRequest{CobsV: []pixapi.LoteCobVRequestItem{{CobVRequest: validCobV(), TxID: tx}}}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := validCobV()
	bad.Chave = ""
	l.CobsV = append(l.CobsV, pixapi.LoteCobVRequestItem{CobVRequest: validCobV(), TxID: tx}, pixapi.LoteCobVRequestItem{CobVRequest: bad, TxID: "short"})
	got := props(l.Validate())
	if fmt.Sprint(got) != "[cobsv[1].txid cobsv[2].txid cobsv[2].chave]" {
		t.Errorf("violations = %v", got)
	}
	empty := pixapi.LoteCobVRequest{}
	if got := props(empty.Validate()); fmt.Sprint(got) != "[cobsv]" {
		t.Errorf("violations = %v", got)
	}
	if !strings.Contains(empty.Validate().Error(), "cobsv: must not be empty") {
		t.Errorf("message = %q", empty.Validate())
	}
}

func ExampleValidateTxID() {
	fmt.Println(pixapi.ValidateTxID("7978c0c97ea847e78e8849634473c1f1"))
	fmt.Println(pixapi.ValidateTxID("pedido-42"))
	// Output:
	// <nil>
	// txid must have 26 to 35 characters, got 9
}

func ExampleCobRequest_Validate() {
	req := pixapi.CobRequest{
		Devedor: &pixapi.Devedor{CPF: "111.111.111-11"},
		Valor:   pixapi.Valor{Original: pixapi.MustParseAmount("10.00")},
		Chave:   "+55 21 98765-4321",
	}
	err := req.Validate()
	var ve *pixapi.ValidationError
	if errors.As(err, &ve) {
		for _, v := range ve.Violacoes {
			fmt.Printf("%s: %s\n", v.Propriedade, v.Razao)
		}
	}
	fmt.Println("normalized key:", req.Chave)
	// Output:
	// devedor.cpf: is not a valid CPF
	// devedor.nome: is required when a document is present
	// normalized key: +5521987654321
}

func TestStatusAndRefunded(t *testing.T) {
	if pixapi.StatusAtiva.Final() || !pixapi.StatusConcluida.Final() || pixapi.Status("").Final() {
		t.Error("Status.Final")
	}
	p := pixapi.Pix{Valor: 1000, Devolucoes: []pixapi.Devolucao{
		{Valor: 100, Status: pixapi.DevolucaoDevolvido},
		{Valor: 200, Status: pixapi.DevolucaoEmProcessamento},
		{Valor: 400, Status: pixapi.DevolucaoNaoRealizado},
	}}
	if p.Refunded() != 300 {
		t.Errorf("Refunded = %s", p.Refunded())
	}
}
