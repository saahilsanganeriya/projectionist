package tmdb

import (
	"context"
	"fmt"
)

// Person is a film's crew member.
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
