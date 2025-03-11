package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/html"
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
			TLSHandshakeTimeout: 60 * time.Second,
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
func decodeEmailBody(msg *gmail.Message) string {
	for _, part := range msg.Payload.Parts {
		if part.MimeType == "text/plain" {
			data, err := base64.URLEncoding.DecodeString(part.Body.Data)
			if err == nil {
				return string(data)
			}
		} else if part.MimeType == "text/html" {
			data, err := base64.URLEncoding.DecodeString(part.Body.Data)
			if err == nil {
				return extractTextFromHTML(string(data))
			}
		}
	}
	if msg.Payload.Body != nil && msg.Payload.Body.Data != "" {
		data, err := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
		if err == nil {
			return extractTextFromHTML(string(data))
		}
	}
	return "No readable content"
}

func extractTextFromHTML(htmlStr string) string {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return htmlStr
	}
	var text string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			text += n.Data + " "
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return strings.TrimSpace(text)
}
func extractEmailInfo(msg *gmail.Message) (string, string, string, string, []string) {
	var from, subject, date, body string
	for _, header := range msg.Payload.Headers {
		switch header.Name {
		case "From":
			from = header.Value
		case "Subject":
			subject = header.Value
		case "Date":
			date = header.Value
		}
	}
	body = decodeEmailBody(msg)
	labels := msg.LabelIds
	return from, subject, date, body, labels
}

type Prompt struct {
	Text string `json:"text"`
}

type GeminiRequest struct {
	Model  string `json:"model"`
	Prompt Prompt `json:"prompt"`
}

type GeminiResponse struct {
	Candidates []struct {
		Output string `json:"output"`
	} `json:"candidates"`
}

func callGemini(prompt string) (string, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY not set")
	}

	reqBody := GeminiRequest{
		Model: "gemini-pro",
		Prompt: Prompt{
			Text: prompt,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", "https://generativelanguage.googleapis.com/v1/models/gemini-pro:generateText?key="+apiKey, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Gemini API: %v", err)
	}
	defer resp.Body.Close()

	var result GeminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %v", err)
	}
	if len(result.Candidates) == 0 {
		return "", fmt.Errorf("no response from Gemini")
	}
	return result.Candidates[0].Output, nil
}

func analyzeEmailWithGemini(body string) (string, error) {
	prompt := fmt.Sprintf("Analyze the following email content and summarize its key points in a concise manner:\n\n%s", body)
	summary, err := callGemini(prompt)
	if err != nil {
		return "", fmt.Errorf("failed to analyze email: %v", err)
	}
	return summary, nil
}

func generateReplyWithGemini(body string) (string, error) {
	prompt := fmt.Sprintf("Generate a polite and professional reply for the following email, if this email is an advertisement or promotion, then please include where the email was sent from and the purpose:\n\n%s", body)
	reply, err := callGemini(prompt)
	if err != nil {
		return "", fmt.Errorf("failed to generate reply: %v", err)
	}
	return reply, nil
}
func main() {
	r := gin.Default()
	setupGmailService()
	r.GET("/emails", func(c *gin.Context) {
		emails, err := listEmails(10)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var wg sync.WaitGroup
		detailedEmailsChan := make(chan struct {
			ID      string   `json:"id"`
			From    string   `json:"from"`
			Subject string   `json:"subject"`
			Date    string   `json:"date"`
			Body    string   `json:"body"`
			Labels  []string `json:"labels"`
			Summary string   `json:"summary"`
			Reply   string   `json:"reply"`
		}, len(emails))

		for _, email := range emails {
			wg.Add(1)
			go func(email *gmail.Message) {
				defer wg.Done()
				msg, err := getEmails(email.Id)
				if err != nil {
					return
				}
				from, subject, date, body, labels := extractEmailInfo(msg)
				summary, err := analyzeEmailWithGemini(body)
				if err != nil {
					summary = "Failed to analyze email: " + err.Error()
				}
				reply, err := generateReplyWithGemini(body)
				if err != nil {
					reply = "Failed to generate reply: " + err.Error()
				}
				detailedEmailsChan <- struct {
					ID      string   `json:"id"`
					From    string   `json:"from"`
					Subject string   `json:"subject"`
					Date    string   `json:"date"`
					Body    string   `json:"body"`
					Labels  []string `json:"labels"`
					Summary string   `json:"summary"`
					Reply   string   `json:"reply"`
				}{email.Id, from, subject, date, body, labels, summary, reply}
			}(email)
		}

		go func() {
			wg.Wait()
			close(detailedEmailsChan)
		}()

		var detailedEmails []struct {
			ID      string   `json:"id"`
			From    string   `json:"from"`
			Subject string   `json:"subject"`
			Date    string   `json:"date"`
			Body    string   `json:"body"`
			Labels  []string `json:"labels"`
			Summary string   `json:"summary"`
			Reply   string   `json:"reply"`
		}
		for email := range detailedEmailsChan {
			detailedEmails = append(detailedEmails, email)
		}
		c.IndentedJSON(http.StatusOK, detailedEmails)
	})
	fmt.Println("Server started on http://localhost:8080")
	r.Run(":8080")
}
