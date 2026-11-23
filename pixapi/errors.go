package pixapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrorTypeBase is the prefix of the problem "type" URIs defined by the
// API Pix.
const ErrorTypeBase = "https://pix.bcb.gov.br/api/v2/error/"

// Problem "type" URIs. The generic ones follow the status codes; the
// resource-specific ones are named after the resource. The exact catalogue
// varies between revisions of the specification: verify against the current
// BCB spec version before matching on a specific URI. Prefer errors.Is with
// the Err* sentinels, which only depend on the HTTP status.
const (
	TypeRequisicaoInvalida        = ErrorTypeBase + "RequisicaoInvalida"
	TypeAcessoNegado              = ErrorTypeBase + "AcessoNegado"
	TypeNaoEncontrado             = ErrorTypeBase + "NaoEncontrado"
	TypeErroInternoDoServidor     = ErrorTypeBase + "ErroInternoDoServidor"
	TypeServicoIndisponivel       = ErrorTypeBase + "ServicoIndisponivel"
	TypeCobOperacaoInvalida       = ErrorTypeBase + "CobOperacaoInvalida"
	TypeCobNaoEncontrado          = ErrorTypeBase + "CobNaoEncontrado"
	TypeCobVOperacaoInvalida      = ErrorTypeBase + "CobVOperacaoInvalida"
	TypeCobVNaoEncontrada         = ErrorTypeBase + "CobVNaoEncontrada"
	TypeLoteCobVOperacaoInvalida  = ErrorTypeBase + "LoteCobVOperacaoInvalida"
	TypeLoteCobVNaoEncontrado     = ErrorTypeBase + "LoteCobVNaoEncontrado"
	TypePixNaoEncontrado          = ErrorTypeBase + "PixNaoEncontrado"
	TypeDevolucaoOperacaoInvalida = ErrorTypeBase + "DevolucaoOperacaoInvalida" // verify against spec
	TypeDevolucaoNaoEncontrada    = ErrorTypeBase + "DevolucaoNaoEncontrada"    // verify against spec
	TypeWebhookOperacaoInvalida   = ErrorTypeBase + "WebhookOperacaoInvalida"
	TypeWebhookNaoEncontrado      = ErrorTypeBase + "WebhookNaoEncontrado"
	TypeLocOperacaoInvalida       = ErrorTypeBase + "PayloadLocationOperacaoInvalida" // verify against spec
	TypeLocNaoEncontrado          = ErrorTypeBase + "PayloadLocationNaoEncontrado"    // verify against spec
)

// Violacao is one entry of a problem's "violacoes": a property that failed
// validation and why.
type Violacao struct {
	Razao       string `json:"razao"`
	Propriedade string `json:"propriedade"`
	Valor       string `json:"valor,omitempty"`
}

// Problem is an RFC 7807 problem details object as used by the API Pix.
type Problem struct {
	Type          string     `json:"type"`
	Title         string     `json:"title"`
	Status        int        `json:"status"`
	Detail        string     `json:"detail,omitempty"`
	CorrelationID string     `json:"correlationId,omitempty"`
	Violacoes     []Violacao `json:"violacoes,omitempty"`
}

// Sentinel errors matched by *Error through errors.Is, based on the HTTP
// status of the response.
var (
	ErrBadRequest   = errors.New("pixapi: bad request")         // 400
	ErrUnauthorized = errors.New("pixapi: unauthorized")        // 401
	ErrForbidden    = errors.New("pixapi: forbidden")           // 403
	ErrNotFound     = errors.New("pixapi: not found")           // 404
	ErrConflict     = errors.New("pixapi: conflict")            // 409
	ErrGone         = errors.New("pixapi: gone")                // 410
	ErrRateLimited  = errors.New("pixapi: rate limited")        // 429
	ErrServer       = errors.New("pixapi: server error")        // 5xx
	ErrUnavailable  = errors.New("pixapi: service unavailable") // 503
)

// Error is returned when the PSP answers with a non-2xx status. When the
// body is a problem details document, Problem is filled in; otherwise Body
// holds up to 4 KiB of the raw response.
type Error struct {
	Problem
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Method and Path identify the request (the path never includes the
	// query string, which may carry personal data such as a CPF).
	Method, Path string
	// RequestID is the value of the request ID header sent with the call.
	RequestID string
	// Body is the raw body when it was not a problem document.
	Body []byte
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pixapi: %s %s: %d", e.Method, e.Path, e.StatusCode)
	if e.Title != "" {
		b.WriteString(" " + e.Title)
	} else if t := http.StatusText(e.StatusCode); t != "" {
		b.WriteString(" " + t)
	}
	if e.Type != "" {
		b.WriteString(" (" + strings.TrimPrefix(e.Type, ErrorTypeBase) + ")")
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	for i, v := range e.Violacoes {
		if i == 0 {
			b.WriteString(" [")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(v.Propriedade + ": " + v.Razao)
		if i == len(e.Violacoes)-1 {
			b.WriteString("]")
		}
	}
	return b.String()
}

// Is matches the Err* sentinels by status code.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrGone:
		return e.StatusCode == http.StatusGone
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrUnavailable:
		return e.StatusCode == http.StatusServiceUnavailable
	case ErrServer:
		return e.StatusCode >= 500
	}
	return false
}

// HasType reports whether err is an *Error whose problem type equals
// typeURI. A bare name such as "CobNaoEncontrado" is matched against the
// last path segment, which tolerates PSPs that use another base URI.
func HasType(err error, typeURI string) bool {
	var e *Error
	if !errors.As(err, &e) || e.Type == "" {
		return false
	}
	if e.Type == typeURI {
		return true
	}
	name := typeURI[strings.LastIndexByte(typeURI, '/')+1:]
	return e.Type[strings.LastIndexByte(e.Type, '/')+1:] == name
}

// ValidationError is returned, before any request is sent, when a request
// does not satisfy the constraints this package checks locally. Its
// Violacoes use the same vocabulary as the PSP's problem documents.
type ValidationError struct {
	Violacoes []Violacao
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Violacoes))
	for i, v := range e.Violacoes {
		parts[i] = v.Propriedade + ": " + v.Razao
	}
	return "pixapi: invalid request: " + strings.Join(parts, "; ")
}

// Is lets errors.Is(err, ErrInvalidRequest) match validation errors.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidRequest }

// ErrInvalidRequest is matched by every *ValidationError.
var ErrInvalidRequest = errors.New("pixapi: invalid request")

// OAuthError is an RFC 6749 error response from the token endpoint.
type OAuthError struct {
	StatusCode  int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// Error implements the error interface.
func (e *OAuthError) Error() string {
	s := fmt.Sprintf("pixapi: token endpoint: %d", e.StatusCode)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}
