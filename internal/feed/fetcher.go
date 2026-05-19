package feed

import "context"

// Fetcher renders a slice of the feed starting at `cursor`. limit is the
// *requested* size; implementations are free to return fewer items (e.g. at
// end of feed) but must never return more.
//
// Why an interface (instead of, say, a type switch in the handler): the pull
// vs push decision is made once at startup and never per-request, so the
// handler should not care which implementation it's calling. The factory
// (NewFetcher in app.go) is the single place that knows.
type Fetcher interface {
	Fetch(ctx context.Context, cursor Cursor, limit int) (Page, error)
}
