package pixapi

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

// Iterator walks the pages of a list endpoint, fetching them lazily.
//
//	it := client.ListCob(ctx, pixapi.CobQuery{Inicio: start, Fim: end})
//	for it.Next() {
//		cob := it.Value()
//		...
//	}
//	if err := it.Err(); err != nil {
//		...
//	}
//
// (The module targets Go 1.22, so range-over-func iterators are not used.)
type Iterator[T any] struct {
	ctx   context.Context
	fetch func(ctx context.Context, page int) ([]T, Paginacao, error)

	page    int
	buf     []T
	i       int
	cur     T
	err     error
	done    bool
	started bool
	pag     Paginacao
}

func newIterator[T any](ctx context.Context, firstPage int, fetch func(context.Context, int) ([]T, Paginacao, error)) *Iterator[T] {
	return &Iterator[T]{ctx: ctx, fetch: fetch, page: firstPage}
}

func errIterator[T any](err error) *Iterator[T] {
	return &Iterator[T]{err: err, done: true}
}

// Next advances to the next item, fetching the next page when needed. It
// returns false when the items are exhausted or an error occurred.
func (it *Iterator[T]) Next() bool {
	for {
		if it.i < len(it.buf) {
			it.cur = it.buf[it.i]
			it.i++
			return true
		}
		if it.done || it.err != nil {
			return false
		}
		if it.started {
			it.page++
		}
		it.started = true
		items, pag, err := it.fetch(it.ctx, it.page)
		if err != nil {
			it.err = err
			return false
		}
		it.pag = pag
		it.buf, it.i = items, 0
		if len(items) == 0 || pag.QuantidadeDePaginas <= it.page+1 {
			it.done = true
		}
	}
}

// Value returns the current item.
func (it *Iterator[T]) Value() T { return it.cur }

// Err returns the error that stopped the iteration, if any.
func (it *Iterator[T]) Err() error { return it.err }

// Paginacao returns the pagination metadata of the last page fetched.
func (it *Iterator[T]) Paginacao() Paginacao { return it.pag }

// All drains the iterator into a slice.
func (it *Iterator[T]) All() ([]T, error) {
	var out []T
	for it.Next() {
		out = append(out, it.Value())
	}
	return out, it.Err()
}

// PageQuery holds the common parameters of list endpoints.
type PageQuery struct {
	// Inicio and Fim bound the creation time; both are required.
	Inicio, Fim time.Time
	// PaginaAtual is the first page to fetch (0-based).
	PaginaAtual int
	// ItensPorPagina is the page size; zero lets the PSP decide.
	ItensPorPagina int
}

func (q PageQuery) values() url.Values {
	v := url.Values{}
	if !q.Inicio.IsZero() {
		v.Set("inicio", q.Inicio.UTC().Format(time.RFC3339))
	}
	if !q.Fim.IsZero() {
		v.Set("fim", q.Fim.UTC().Format(time.RFC3339))
	}
	if q.ItensPorPagina > 0 {
		v.Set("paginacao.itensPorPagina", strconv.Itoa(q.ItensPorPagina))
	}
	return v
}

func (q PageQuery) validate(requireRange bool) error {
	var v validator
	if requireRange && q.Inicio.IsZero() {
		v.add("inicio", "is required")
	}
	if requireRange && q.Fim.IsZero() {
		v.add("fim", "is required")
	}
	if !q.Inicio.IsZero() && !q.Fim.IsZero() && q.Fim.Before(q.Inicio) {
		v.add("fim", "must not be before inicio")
	}
	if q.PaginaAtual < 0 {
		v.add("paginacao.paginaAtual", "must not be negative")
	}
	if q.ItensPorPagina < 0 {
		v.add("paginacao.itensPorPagina", "must not be negative")
	}
	return v.err()
}

// list builds an iterator over a list endpoint whose items are decoded by
// extract from the page envelope E.
func list[T any, E any](c *Client, ctx context.Context, path string, q PageQuery, extra url.Values, requireRange bool, extract func(*E) ([]T, Parametros)) *Iterator[T] {
	if err := q.validate(requireRange); err != nil {
		return errIterator[T](err)
	}
	return newIterator(ctx, q.PaginaAtual, func(ctx context.Context, page int) ([]T, Paginacao, error) {
		v := q.values()
		for k, vs := range extra {
			v[k] = vs
		}
		v.Set("paginacao.paginaAtual", strconv.Itoa(page))
		var env E
		if err := c.Do(ctx, "GET", path, v, nil, &env); err != nil {
			return nil, Paginacao{}, err
		}
		items, p := extract(&env)
		return items, p.Paginacao, nil
	})
}
