package pixapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"time"

	"github.com/victorcondackdev/brpay/pix"
	"github.com/victorcondackdev/pix-cobranca-go/mockpsp"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

// mock starts an in-process mock PSP over plain HTTP. Real integrations
// pass the PSP's URL and WithTLSConfig(LoadClientTLS(...)).
func mock() (*httptest.Server, func()) {
	psp, _ := mockpsp.New(mockpsp.Config{PublicURL: "https://pix.example.com"})
	ts := httptest.NewServer(psp.Handler())
	return ts, func() { ts.Close(); psp.Close() }
}

func ExampleClient_CreateCob() {
	ts, stop := mock()
	defer stop()

	client, err := pixapi.NewClient(ts.URL+"/api/v2",
		// pixapi.WithTLSConfig(tlsConfig), // mTLS with the PSP-issued certificate
		pixapi.WithOAuth2(ts.URL+"/oauth/token", "dev-client", "dev-secret", "cob.write", "cob.read"),
	)
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	cob, err := client.CreateCob(ctx, "pedido42abcdefghijklmnopqrstu", pixapi.CobRequest{
		Calendario:         &pixapi.Calendario{Expiracao: 3600},
		Devedor:            &pixapi.Devedor{CPF: "123.456.789-09", Nome: "Fulano de Tal"},
		Valor:              pixapi.Valor{Original: pixapi.MustParseAmount("37.00")},
		Chave:              "+55 21 98765-4321",
		SolicitacaoPagador: "Pedido 42",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(cob.TxID, cob.Status, cob.Valor.Original, cob.Chave)

	code, _ := pix.Decode(cob.PixCopiaECola)
	fmt.Println("dynamic BR Code:", code.Dynamic, code.URL == cob.Location)

	_, err = client.GetCob(ctx, "naoexisteabcdefghijklmnopqrstu")
	fmt.Println(errors.Is(err, pixapi.ErrNotFound), pixapi.HasType(err, pixapi.TypeCobNaoEncontrado))
	// Output:
	// pedido42abcdefghijklmnopqrstu ATIVA 37.00 +5521987654321
	// dynamic BR Code: true true
	// true true
}

func ExampleClient_ListCob() {
	ts, stop := mock()
	defer stop()
	client, _ := pixapi.NewClient(ts.URL+"/api/v2", pixapi.WithOAuth2(ts.URL+"/oauth/token", "dev-client", "dev-secret"))
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		_, _ = client.CreateCob(ctx, "", pixapi.CobRequest{Valor: pixapi.Valor{Original: pixapi.Amount(i * 100)}, Chave: "loja@example.com"})
	}

	it := client.ListCob(ctx, pixapi.CobQuery{PageQuery: pixapi.PageQuery{
		Inicio:         time.Now().Add(-time.Hour),
		Fim:            time.Now().Add(time.Hour),
		ItensPorPagina: 2,
	}})
	var total pixapi.Amount
	for it.Next() {
		total += it.Value().Valor.Original
	}
	if err := it.Err(); err != nil {
		panic(err)
	}
	fmt.Println(total, it.Paginacao().QuantidadeDePaginas)
	// Output: 6.00 2
}
