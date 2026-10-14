package tmdb

import (
	"context"
	"fmt"
	"net/url"
)

// Person is someone TMDB credits: a film's crew member, a show's creator.
type Person struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Directors lists the people credited as Director on a film.
func (c *Client) Directors(ctx context.Context, movieID int) ([]Person, error) {
	var resp struct {
		Crew []struct {
			Person
			Job string `json:"job"`
		} `json:"crew"`
	}
	if err := c.get(ctx, fmt.Sprintf("/movie/%d/credits", movieID), nil, &resp); err != nil {
		return nil, err
	}
	var out []Person
	for _, p := range resp.Crew {
		if p.Job == "Director" {
			out = append(out, p.Person)
		}
	}
	return out, nil
}

// Taste is what a film is made of, and what TMDB's users watch with it: its details, its cast in billing order, its
// crew, its keywords, and the films TMDB recommends from it (the first page, best first).
type Taste struct {
	Details
	Cast            []Person
	Crew            []Credit
	Keywords        []string
	Recommendations []int
}

type Credit struct {
	Person
	Job string `json:"job"`
}

// MovieTaste is a film's Taste, in one request.
func (c *Client) MovieTaste(ctx context.Context, movieID int) (Taste, error) {
	var raw struct {
		Details
		Credits struct {
			Cast []Person `json:"cast"`
			Crew []Credit `json:"crew"`
		} `json:"credits"`
		Keywords struct {
			Keywords []struct {
				Name string `json:"name"`
			} `json:"keywords"`
		} `json:"keywords"`
		Recommendations struct {
			Results []struct {
				ID int `json:"id"`
			} `json:"results"`
		} `json:"recommendations"`
	}
	q := url.Values{"append_to_response": {"credits,keywords,recommendations"}}
	if err := c.get(ctx, fmt.Sprintf("/movie/%d", movieID), q, &raw); err != nil {
		return Taste{}, err
	}
	t := Taste{Details: raw.Details, Cast: raw.Credits.Cast, Crew: raw.Credits.Crew}
	t.Kind = Movie
	for _, k := range raw.Keywords.Keywords {
		t.Keywords = append(t.Keywords, k.Name)
	}
	for _, r := range raw.Recommendations.Results {
		t.Recommendations = append(t.Recommendations, r.ID)
	}
	return t, nil
}

// Directed lists the TMDB ids of the films a person is credited as Director on.
func (c *Client) Directed(ctx context.Context, personID int) ([]int, error) {
	var resp struct {
		Crew []struct {
			ID  int    `json:"id"`
			Job string `json:"job"`
		} `json:"crew"`
	}
	if err := c.get(ctx, fmt.Sprintf("/person/%d/movie_credits", personID), nil, &resp); err != nil {
		return nil, err
	}
	var out []int
	for _, f := range resp.Crew {
		if f.Job == "Director" {
			out = append(out, f.ID)
		}
	}
	return out, nil
}

// PersonImages lists a person's profile photos (portraits).
func (c *Client) PersonImages(ctx context.Context, personID int) ([]Image, error) {
	var resp struct {
		Profiles []Image `json:"profiles"`
	}
	err := c.get(ctx, fmt.Sprintf("/person/%d/images", personID), nil, &resp)
	return resp.Profiles, err
}

// TaggedImage is an image a person is tagged in: a still, a backdrop or a poster of a film or show.
type TaggedImage struct {
	Image
	ImageType string `json:"image_type"` // still | backdrop | poster | ...
	Media     struct {
		Title string `json:"title"` // films
		Name  string `json:"name"`  // shows
	} `json:"media"`
}

// TaggedImages lists the first page (TMDB's 20 best) of the images a person is tagged in.
func (c *Client) TaggedImages(ctx context.Context, personID int) ([]TaggedImage, error) {
	var resp struct {
		Results []TaggedImage `json:"results"`
	}
	err := c.get(ctx, fmt.Sprintf("/person/%d/tagged_images", personID), nil, &resp)
	return resp.Results, err
}

// PersonExternalIDs is a person's ids elsewhere; Wikidata is the one Wikimedia Commons keys its files by.
func (c *Client) PersonExternalIDs(ctx context.Context, personID int) (ExternalIDs, error) {
	var out ExternalIDs
	err := c.get(ctx, fmt.Sprintf("/person/%d/external_ids", personID), nil, &out)
	return out, err
}
