package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
)

type ImgurClient struct {
	http *http.Client
}

func (i ImgurClient) GetAlbum(id string) (Album, error) {
	u := fmt.Sprintf(`https://imgur.com/ajaxalbums/getimages/%s`, id)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return Album{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reddit image downloader")

	resp, err := i.http.Do(req)
	if err != nil {
		return Album{}, err
	}
	defer func() {
		_, _ = io.Copy(ioutil.Discard, resp.Body)
		err := resp.Body.Close()
		if err != nil {
			log.Printf("error closing response body: %v", err)
		}
	}()
	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return Album{}, err
	}
	var album Album
	err = json.Unmarshal(body, &album)
	return album, err
}

type Album struct {
	AlbumData `json:"data"`
	Success   bool
	Status    int
}

type AlbumData struct {
	Count  int
	Images []AlbumImage
}

type AlbumImage struct {
	Hash     string
	Title    string
	Ext      string
	Datetime string
}
