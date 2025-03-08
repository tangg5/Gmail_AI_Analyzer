package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

var srv *gmail.Service

func setupGmailService() {
	ctx := context.Background()

	//read credentials.json
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read credentials.json: %v", err)
	}

	//setup OAuth2
	config, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope)
	if err != nil {
		log.Fatalf("Unable to parse credentials: %v", err)
	}
	//acquire token
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	var client *http.Client
	maxAttempts := 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		transport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   600 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 60 * time.Second, // 添加TLS握手超时
			DisableKeepAlives:   false,
		}
		client = &http.Client{
			Transport: transport,
			Timeout:   600 * time.Second,
		}

		//create Gmail Service
		client = config.Client(ctx, tok)
		srv, err = gmail.NewService(ctx, option.WithHTTPClient(client))
		if err == nil {
			// log.Fatalf("Unable to create Gmail service: %v", err)
			break
		}
		if attempt < maxAttempts {
			log.Printf("Attempt %d to create Gmail service failed: %v. Retrying in 10 seconds...", attempt, err)
			time.Sleep(10 * time.Second)
		}

	}
	if err != nil {
		log.Fatalf("Unable to create Gmail service after %d retries: %v", maxAttempts, err)
	}
}

func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	fmt.Printf("Go to this link and authorize: %v\nThen paste the code here: ", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code: %v", err)
	}
	fmt.Println("Authorization code received:", authCode)

	var tok *oauth2.Token
	maxAttempts := 5
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var err error
		transport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   600 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 60 * time.Second,
			DisableKeepAlives:   false,
		}
		client := &http.Client{
			Transport: transport,
			Timeout:   600 * time.Second,
		}
		fmt.Println("Authorization code received.")
		tok, err = config.Exchange(context.WithValue(context.TODO(), oauth2.HTTPClient, client), authCode)
		if err == nil {
			fmt.Println("Token received successfully.")
			return tok
		}
		lastErr = err
		log.Printf("Attempt %d to exchange token failed: %v. Retrying in 10 seconds...", attempt, err)
		if attempt < maxAttempts {
			time.Sleep(10 * time.Second)
		}
	}
	log.Fatalf("Unable to acquire token after %d retries: %v", maxAttempts, lastErr)
	return nil
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Unable to save token: %v", err)

	}
	defer f.Close()
	err = json.NewEncoder(f).Encode(token)
	if err != nil {
		log.Fatalf("Unable to encodetoken: %v", err)
	}
}

func listEmails(numofemails int) ([]*gmail.Message, error) {
	user := "me"
	r, err := srv.Users.Messages.List(user).MaxResults(int64(numofemails)).Do()
	if err != nil {
		return nil, err
	}
	return r.Messages, nil
}

func getEmails(emailid string) (*gmail.Message, error) {
	user := "me"
	msg, err := srv.Users.Messages.Get(user, emailid).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve emails details: %v", err)
	}
	return msg, nil
}

//	func decodeEmail(msg *gmail.Message) (string, error) {
//		for _, part := range msg.Payload.Parts {
//			if part.MimeType == "text/plain" || part.MimeType == "text/html" {
//				data, _ := base64.URLEncoding.DecodeString(part.Body.Data)
//				return string(data)
//			}
//		}
//		return ""
//	}
func main() {
	r := gin.Default()
	setupGmailService()
	// r.GET("/ping", func(c *gin.Context) {
	// 	c.JSON(http.StatusOK, gin.H{
	// 		"message": "pong",
	// 	})
	// })
	r.GET("/emails", func(c *gin.Context) {
		emails, err := listEmails(10)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, emails)
	})
	fmt.Println("Server started on http://localhost:8080")
	r.Run(":8080")
}
