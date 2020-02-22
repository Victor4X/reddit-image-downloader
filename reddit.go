package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
)

var RateLimited error = errors.New("rate limited")

type RedditClient struct {
	http *http.Client
}

func encodeNewListingParams(params NewListingParams) string {
	q := url.Values{}
	q.Add("raw_json", "1")
	if params.Limit > 0 {
		q.Add("limit", strconv.Itoa(params.Limit))
	}
	if params.Before != "" {
		q.Add("before", params.Before)
	}
	if params.After != "" {
		q.Add("after", params.After)
	}
	return q.Encode()
}

func encodeSearchListingParams(params SearchListingParams) string {
	q := url.Values{}
	q.Add("raw_json", "1")
	q.Add("restrict_sr", "on")
	q.Add("sort", "new")
	if params.Limit > 0 {
		q.Add("limit", strconv.Itoa(params.Limit))
	}
	if params.Before != "" {
		q.Add("before", params.Before)
	}
	if params.After != "" {
		q.Add("after", params.After)
	}
	if params.Search != "" {
		q.Add("q", params.Search)
	}

	return q.Encode()
}

func (r RedditClient) GetSearch(subreddit string, params SearchListingParams) (Listing, error) {
	urlParams := encodeSearchListingParams(params)
	u := fmt.Sprintf(`https://www.reddit.com/r/%s/search.json?%s`, subreddit, urlParams)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return Listing{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reddit image downloader")

	resp, err := r.http.Do(req)
	if err != nil {
		return Listing{}, err
	}
	defer func() {
		_, _ = io.Copy(ioutil.Discard, resp.Body)
		err := resp.Body.Close()
		if err != nil {
			log.Printf("error closing response body: %v", err)
		}
	}()

	if resp.StatusCode == 429 {
		return Listing{}, RateLimited
	}

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return Listing{}, err
	}
	var listing Listing
	err = json.Unmarshal(body, &listing)
	return listing, err
}

func (r RedditClient) GetNew(subreddit string, params NewListingParams) (Listing, error) {
	urlParams := encodeNewListingParams(params)
	u := fmt.Sprintf(`https://www.reddit.com/r/%s/new.json?%s`, subreddit, urlParams)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return Listing{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reddit image downloader")

	resp, err := r.http.Do(req)
	if err != nil {
		return Listing{}, err
	}
	defer func() {
		_, _ = io.Copy(ioutil.Discard, resp.Body)
		err := resp.Body.Close()
		if err != nil {
			log.Printf("error closing response body: %v", err)
		}
	}()

	if resp.StatusCode == 429 {
		return Listing{}, RateLimited
	}

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return Listing{}, err
	}
	var listing Listing
	err = json.Unmarshal(body, &listing)
	return listing, err
}

type NewListingParams struct {
	Limit  int
	Before string
	After  string
}

type SearchListingParams struct {
	Limit  int
	Before string
	After  string
	Search string
}

type Listing struct {
	Kind        string
	ListingData `json:"data"`
}

type ListingData struct {
	Modhash  string
	Dist     int
	Children []Submission
	After    string
	Before   string
}

type Submission struct {
	Kind           string
	SubmissionData `json:"data"`
}

type SubmissionData struct {
	// uninteresting members are omitted
	Title      string
	Name       string
	Id         string
	IsMeta     bool   `json:"is_meta"`
	PostHint   string `json:"post_hint"`
	Domain     string
	Author     string
	CreatedUtc float64 `json:"created_utc"`
	Url        string
	Permalink  string
	Subreddit  string
	Nsfw       bool `json:"over_18"`
}
