package tmdb

import (
	"context"
	"net/url"
)

// Find maps an external id to TMDB's films and shows; source is "imdb_id" ("tt0113277") or "tvdb_id" ("81189").
func (c *Client) Find(ctx context.Context, id, source string) (movies, shows []Result, err error) {
	var resp struct {
		Movies []Result `json:"movie_results"`
		Shows  []struct {
			Result
			showSpelling
		} `json:"tv_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(id), url.Values{"external_source": {source}}, &resp); err != nil {
		return nil, nil, err
	}
	shows = make([]Result, len(resp.Shows))
	for i, s := range resp.Shows {
		shows[i] = s.Result
		s.fromShow(&shows[i].Title, &shows[i].OriginalTitle, &shows[i].Date)
	}
	return resp.Movies, shows, nil
}
