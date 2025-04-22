package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/html"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}
var redisClient *redis.Client
var srv *gmail.Service
var (
	emailRe = regexp.MustCompile(`(?i)\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	phoneRe = regexp.MustCompile(`(?i)\b(\+?\d{1,3}[-.\s]?)?(\d{3}[-.\s]?\d{3}[-.\s]?\d{4})\b`)
	nameRe  = regexp.MustCompile(`(?i)\b([A-Z][a-z]+)\s([A-Z][a-z]+)\b`)
	ccRe    = regexp.MustCompile(`\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b`)
)

func watchInbox(userID string, topicName string) (string, int64, error) {
	ctx := context.Background()
	if srv == nil {
		return "", 0, fmt.Errorf("Gmail service not initialized")
	}

	req := &gmail.WatchRequest{
		LabelIds:  []string{"INBOX"},
		TopicName: topicName,
	}

	resp, err := srv.Users.Watch(userID, req).Context(ctx).Do()
	if err != nil {
		return "", 0, fmt.Errorf("failed to set up watch: %v", err)
	}

	log.Printf("Watch set up successfully: HistoryID=%d, Expiration=%d", resp.HistoryId, resp.Expiration)
	return fmt.Sprintf("%d", resp.HistoryId), resp.Expiration, nil
}

func subscribeToPubSub(ctx context.Context, redisClient *redis.Client, projectID, subscriptionID string) error {
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		return fmt.Errorf("failed to create Pub/Sub client: %v", err)
	}

	sub := client.Subscription(subscriptionID)
	exists, err := sub.Exists(ctx)
	if err != nil {
		client.Close()
		return fmt.Errorf("failed to check subscription existence: %v", err)
	}
	if !exists {
		client.Close()
		return fmt.Errorf("subscription %s does not exist", subscriptionID)
	}

	log.Printf("Subscribing to Pub/Sub subscription: %s", subscriptionID)
	go func() {
		var lastHistoryId int64
		for {
			log.Printf("Waiting for Pub/Sub messages on subscription: %s", subscriptionID)
			err := sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
				log.Printf("Received Pub/Sub message: %s", string(msg.Data))

				var gmailData struct {
					EmailAddress string `json:"emailAddress"`
					HistoryId    int64  `json:"historyId"`
				}
				if err := json.Unmarshal(msg.Data, &gmailData); err != nil {
					log.Printf("Failed to unmarshal Gmail data: %v", err)
					msg.Nack()
					return
				}
				startHistoryId := lastHistoryId
				if startHistoryId == 0 || startHistoryId >= gmailData.HistoryId {
					startHistoryId = gmailData.HistoryId - 200
				}
				log.Printf("Querying history from StartHistoryId: %d to %d", startHistoryId, gmailData.HistoryId)
				var history *gmail.ListHistoryResponse
				for attempt := 1; attempt <= 3; attempt++ {
					history, err = srv.Users.History.List("me").StartHistoryId(uint64(startHistoryId)).LabelId("INBOX").Do()
					if err != nil {
						log.Printf("Attempt %d: Failed to list history: %v", attempt, err)
						msg.Nack()
						return
					}
					if len(history.History) > 0 {
						break
					}
					log.Printf("Attempt %d: No history records found, retrying in 2 seconds...", attempt)
					time.Sleep(2 * time.Second)
				}

				hasNewMessage := false
				if len(history.History) == 0 {
					log.Printf("No history records found for HistoryId: %d", gmailData.HistoryId)
				}
				for _, h := range history.History {
					for _, m := range h.MessagesAdded {
						log.Printf("Found Added messages, Message ID: %s, History ID: %d", m.Message.Id, h.Id)
						hasNewMessage = true
					}
					// for _, m := range h.MessagesDeleted {
					// 	log.Printf("Found Deleted email, Message ID: %s, History ID: %d", m.Message.Id, h.Id)
					// }
					// for _, m := range h.LabelsAdded {
					// 	log.Printf("Found Labels-added-to-Message email ID: %s, Labels: %v, History ID: %d", m.Message.Id, m.LabelIds, h.Id)
					// }
					// for _, m := range h.LabelsRemoved {
					// 	log.Printf("Found Labels-removed email from Message ID: %s, Labels: %v, History ID: %d", m.Message.Id, m.LabelIds, h.Id)
					// }

				}

				if hasNewMessage {
					log.Printf("New email for %s, History Id: %d", gmailData.EmailAddress, gmailData.HistoryId)
					log.Printf("Publishing message to Redis Channel: new_email_channel")
					var messageIds []string
					for _, h := range history.History {
						for _, m := range h.MessagesAdded {
							messageIds = append(messageIds, m.Message.Id)
						}
					}
					publishDataStruct := struct {
						EmailAddress string   `json:"emailAddress"`
						HistoryId    int64    `json:"historyId"`
						MessageIds   []string `json:"messageIds"`
					}{
						EmailAddress: gmailData.EmailAddress,
						HistoryId:    gmailData.HistoryId,
						MessageIds:   messageIds,
					}
					publishData, err := json.Marshal(publishDataStruct)
					if err != nil {
						log.Printf("Failed to marshal publish data: %v", err)
						msg.Nack()
						return
					}
					result := redisClient.Publish(ctx, "new_email_channel", string(publishData))
					log.Printf("Redis publish resule: %v", result)
				} else {
					log.Printf("No new email, ignoring HistoryId: %d", gmailData.HistoryId)
				}
				lastHistoryId = gmailData.HistoryId
				msg.Ack()

				// log.Printf("New email for %s, HistoryId: %d", gmailData.EmailAddress, gmailData.HistoryId)
				// log.Printf("Publishing message to Redis channel: new_email_channel")
				// publishData, err := json.Marshal(gmailData)
				// if err != nil {
				// 	log.Printf("Failed to marshal publish data: %v", err)
				// 	msg.Nack()
				// 	return
				// }

				// result := redisClient.Publish(ctx, "new_email_channel", string(publishData))
				// log.Printf("Redis publish result: %v", result)
				// msg.Ack()
			})
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				if ctx.Err() != nil {
					client.Close()
					return
				}
				time.Sleep(5 * time.Second)
				continue
			}
		}
	}()

	return nil
}

func initRedis() error {
	redisClient = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})
	ctx := context.Background()
	maxAttempts := 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, err := redisClient.Ping(ctx).Result()
		if err == nil {
			log.Println("Connected to Redis successfully")
			return nil
		}
		log.Printf("Attempt %d to connect to Redis failed: %v. Retrying in 5 seconds...", attempt, err)
		if attempt == maxAttempts {
			return fmt.Errorf("failed to connect to Redis after %d attempts: %v", maxAttempts, err)
		}
		time.Sleep(5 * time.Second)
	}
	return nil
}

func setupGmailService() {
	ctx := context.Background()

	//read credentials.json
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Printf("Unable to read credentials.json: %v", err)
	}

	//setup OAuth2
	config, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope)
	if err != nil {
		log.Printf("Unable to parse credentials: %v", err)
		return
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
		log.Printf("Unable to create Gmail service after %d retries: %v", maxAttempts, err)
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

func listEmails(numofemails int) ([]*gmail.Message, string, error) {
	user := "me"
	r, err := srv.Users.Messages.List(user).MaxResults(int64(numofemails)).Do()
	if err != nil {
		return nil, "", err
	}
	return r.Messages, r.NextPageToken, nil
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

type GeminiRequest struct {
	Contents []Content `json:"contents"`
}

type Content struct {
	Parts []Part `json:"parts"`
}

type Part struct {
	Text string `json:"text"`
}

type GeminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func callGemini(prompt string) (string, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Println("GEMINI_API_KEY is not set")
		return "", fmt.Errorf("GEMINI_API_KEY not set")
	}
	log.Printf("Calling Gemini API with prompt: %s", prompt)

	reqBody := GeminiRequest{
		Contents: []Content{
			{
				Parts: []Part{
					{
						Text: prompt,
					},
				},
			},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro-latest:generateContent?key="+apiKey, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Gemini API call failed: %v", err)
		return "", fmt.Errorf("failed to call Gemini API: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("Gemini API status code: %d", resp.StatusCode)
	bodyBytes, _ := io.ReadAll(resp.Body)
	log.Printf("Gemini API raw response: %s", string(bodyBytes))

	var result GeminiResponse
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&result); err != nil {
		log.Printf("Failed to decode Gemini response: %v", err)
		return "", fmt.Errorf("failed to decode response: %v", err)
	}
	if len(result.Candidates) == 0 {
		log.Println("No candidates in Gemini response")
		return "", fmt.Errorf("no response from Gemini")
	}
	return result.Candidates[0].Content.Parts[0].Text, nil
}
func sanitizeContent(from, body string) (string, string) {
	sanitizedFrom := emailRe.ReplaceAllString(from, "[email redacted]")
	sanitizedBody := emailRe.ReplaceAllString(body, "[email redacted]")

	sanitizedBody = phoneRe.ReplaceAllString(sanitizedBody, "[phone redacted]")
	sanitizedBody = ccRe.ReplaceAllString(sanitizedBody, "[credit card redacted]")

	// sanitizedBody = nameRe.ReplaceAllString(sanitizedBody, "[name redacted]")
	// sanitizedBody = ccRe.ReplaceAllString(sanitizedBody, "[credit card redacted]")
	nameRe := regexp.MustCompile(`(?i)(?:Best,|Sincerely,|Regards,|Good Morning,)\s+([A-Z][a-z]+(?:\s+[A-Z][a-z]+)?)`)
	sanitizedBody = nameRe.ReplaceAllString(sanitizedBody, "$0 [name redacted]")

	sanitizedBody = strings.ReplaceAll(sanitizedBody, "\r\n", "\n")
	sanitizedBody = regexp.MustCompile(`\n\s*\n+`).ReplaceAllString(sanitizedBody, "\n")
	sanitizedBody = regexp.MustCompile(`\s+`).ReplaceAllString(sanitizedBody, " ")
	sanitizedBody = strings.TrimSpace(sanitizedBody)

	sanitizedFrom = strings.TrimSpace(sanitizedFrom)
	return sanitizedFrom, sanitizedBody
}
func analyzeEmailWithGemini(from, body string) (string, string, error) {
	sanitizedFrom, sanitizedBody := sanitizeContent(from, body)
	if len(sanitizedBody) > 5000 {
		sanitizedBody = sanitizedBody[:5000] + "... [truncated]"
	}
	prompt := fmt.Sprintf("Analyze the following email content and summarize its key points in a concise manner, if this email is a question or a request, please include the question or request :\n\nFrom: %s\n\n%s", sanitizedFrom, sanitizedBody)
	response, err := callGemini(prompt)
	if err != nil {
		return "", "", fmt.Errorf("failed to analyze email: %v", err)
	}
	parts := strings.SplitN(response, "\nReply: ", 2)
	if len(parts) != 2 {
		return parts[0], "", nil
	}
	return parts[0], parts[1], nil
	// return summary, nil
}

func generateReplyWithGemini(from, body string) (string, error) {
	sanitizedFrom, sanitizedBody := sanitizeContent(from, body)
	prompt := fmt.Sprintf("Generate a polite and professional reply for the following email, if this email is an advertisement or promotion, then do nothing and let it go:\n\nFrom: %s\n\n%s", sanitizedFrom, sanitizedBody)
	reply, err := callGemini(prompt)
	if err != nil {
		return "", fmt.Errorf("failed to generate reply: %v", err)
	}
	return reply, nil
}

func analyzeAndReplyWithGemini(from, body string) (string, string, error) {
	sanitizedFrom, sanitizedBody := sanitizeContent(from, body)
	if len(sanitizedBody) > 2000 {
		sanitizedBody = sanitizedBody[:2000] + "... [truncated]"
	}

	prompt := fmt.Sprintf("Analyze the following email content and summarize its key points in a concise manner, if this email is a question or a request, please include the question or request:\n\nFrom: %s\n\n%s", sanitizedFrom, sanitizedBody)
	summary, err := callGemini(prompt)
	if err != nil {
		return "", "", fmt.Errorf("failed to analyze: %v", err)
	}

	prompt = fmt.Sprintf("Generate a polite and professional reply for the following email, if this email is an advertisement or promotion, then do nothing and let it go:\n\nFrom: %s\n\n%s", sanitizedFrom, sanitizedBody)
	reply, err := callGemini(prompt)
	if err != nil {
		return summary, "", fmt.Errorf("failed to generate reply: %v", err)
	}

	return summary, reply, nil
}

func main() {
	r := gin.Default()
	setupGmailService()
	// initRedis()
	if err := initRedis(); err != nil {
		log.Fatalf("Redis initialization failed: %v", err)
	}

	if srv == nil {
		log.Println("Gmail service not initialized, exiting...")
		return
	}
	if redisClient == nil {
		log.Println("Redis client not initialized, exiting...")
		return
	}

	topicName := "projects/x-ripple-452808-i4/topics/gmail-notifications"
	historyID, expiration, err := watchInbox("me", topicName)
	if err != nil {
		log.Fatalf("Failed to set up Gmail watch: %v", err)
	}
	log.Printf("Gmail watch set up: HistoryID=%s, Expiration=%d", historyID, expiration)

	go func() {
		ticker := time.NewTicker(5 * 24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			newHistoryID, newExpiration, err := watchInbox("me", topicName)
			if err != nil {
				log.Printf("Failed to renew watch: %v", err)
				continue
			}
			historyID = newHistoryID
			expiration = newExpiration
			log.Printf("Watch renewed: HistoryID=%s, Expiration=%d", historyID, expiration)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go subscribeToPubSub(ctx, redisClient, "x-ripple-452808-i4", "gmail-subscription")

	r.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Printf("Failed to upgrade to WebSocket: %v", err)
			return
		}
		defer conn.Close()

		ctx := context.Background()
		pubsub := redisClient.Subscribe(ctx, "new_email_channel")
		defer pubsub.Close()

		ch := pubsub.Channel()
		for msg := range ch {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg.Payload)); err != nil {
				log.Printf("Failed to send WebSocket message: %v", err)
				return
			}
		}
	})
	r.GET("/emails", func(c *gin.Context) {
		ctx := context.Background()
		cacheKey := "emails:latest"
		historyKey := "emails:history"

		cachedData, err := redisClient.Get(ctx, cacheKey).Result()
		cachedHistory, _ := redisClient.Get(ctx, historyKey).Result()

		emails, nextToken, err := listEmails(5)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if err == nil && cachedData != "" && cachedHistory == nextToken {
			var cachedEmails []struct {
				ID      string   `json:"id"`
				From    string   `json:"from"`
				Subject string   `json:"subject"`
				Date    string   `json:"date"`
				Body    string   `json:"body"`
				Labels  []string `json:"labels"`
				Summary string   `json:"summary"`
				Reply   string   `json:"reply"`
			}
			if err := json.Unmarshal([]byte(cachedData), &cachedEmails); err == nil {
				c.IndentedJSON(http.StatusOK, cachedEmails)
				log.Println("Returning cached emails from Redis")
				return
			}
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

		semaphore := make(chan struct{}, 2)

		for _, email := range emails {
			wg.Add(1)
			semaphore <- struct{}{}
			go func(email *gmail.Message) {
				defer wg.Done()
				defer func() { <-semaphore }()

				msg, err := getEmails(email.Id)
				if err != nil {
					log.Printf("Failed to get email %s: %v", email.Id, err)
					detailedEmailsChan <- struct {
						ID      string   `json:"id"`
						From    string   `json:"from"`
						Subject string   `json:"subject"`
						Date    string   `json:"date"`
						Body    string   `json:"body"`
						Labels  []string `json:"labels"`
						Summary string   `json:"summary"`
						Reply   string   `json:"reply"`
					}{email.Id, "", "", "", "", nil, "Failed to fetch email", ""}
					return
				}
				from, subject, date, body, labels := extractEmailInfo(msg)
				sanitizedFrom, sanitizedBody := sanitizeContent(from, body)
				summary, reply, err := analyzeAndReplyWithGemini(sanitizedFrom, sanitizedBody)
				if err != nil {
					summary = "Failed to analyze email: " + err.Error()
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
				}{email.Id, sanitizedFrom, subject, date, sanitizedBody, labels, summary, reply}
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

		jsonData, err := json.Marshal(detailedEmails)
		if err != nil {
			log.Printf("Failed to marshal emails: %v", err)
		} else {
			err = redisClient.Set(ctx, cacheKey, jsonData, 2*time.Hour).Err()
			if err != nil {
				log.Printf("Failed to cache emails in Redis: %v", err)
			} else {
				log.Println("Cached emails in Redis")
			}
			err = redisClient.Set(ctx, historyKey, nextToken, 2*time.Hour).Err()
			if err != nil {
				log.Printf("Failed to cache history in Redis: %v", err)
			}
		}

		c.IndentedJSON(http.StatusOK, detailedEmails)
	})
	fmt.Println("Server started on http://localhost:8080")
	r.Run(":8080")
}
