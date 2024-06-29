package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	proxyUrlTemplate = "http://%s"
	urlToGet         = "http://ip-api.com/json"
	retryInterval    = 10 * time.Second // interval to wait before retrying connection
)

var (
	CustomHeaders = http.Header{
		"User-Agent": []string{
			"Mozilla/5.0 (Windows NT 10.2; WOW64) AppleWebKit/601.9 (KHTML, like Gecko) Chrome/55.0.2426.397 Safari/602.5 Edge/11.23371",
		},
	}
)

func getProxyIP(proxy string) (string, error) {
	proxyURL, err := url.Parse(fmt.Sprintf(proxyUrlTemplate, proxy))
	if err != nil {
		return "", fmt.Errorf("failed to form proxy URL: %v", err)
	}

	u, err := url.Parse(urlToGet)
	if err != nil {
		return "", fmt.Errorf("failed to form GET request URL: %v", err)
	}

	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	client := &http.Client{Transport: transport}

	request, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to form GET request: %v", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("failed to perform GET request: %v", err)
	}
	defer response.Body.Close()

	responseBodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("could not read response body bytes: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(responseBodyBytes, &result); err != nil {
		return "", fmt.Errorf("could not unmarshal response body: %v", err)
	}

	if query, ok := result["query"].(string); ok {
		return query, nil
	}
	return "", fmt.Errorf("query field not found in response")
}

func sendPing(ctx context.Context, c *websocket.Conn, proxyIP string, logger zerolog.Logger) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sendMessage := map[string]interface{}{
				"id":      uuid.New().String(),
				"version": "1.0.0",
				"action":  "PING",
				"data":    map[string]interface{}{},
			}
			err := c.WriteJSON(sendMessage)
			if err != nil {
				logger.Error().Err(err).Msg("Error sending ping")
				return
			}
			logger.Info().Str("ip", proxyIP).Interface("message", sendMessage).Msg("Sent ping message")
		case <-ctx.Done():
			return
		}
	}
}

func receiveMessages(ctx context.Context, c *websocket.Conn, proxyIP, deviceID, userID string, logger zerolog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_, message, err := c.ReadMessage()
			if err != nil {
				logger.Error().Err(err).Msg("Error reading message")
				return
			}

			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err != nil {
				logger.Error().Err(err).Msg("Error unmarshalling message")
				continue
			}
			logger.Info().Str("ip", proxyIP).Interface("message", msg).Msg("Received message")

			switch action := msg["action"].(string); action {
			case "AUTH":
				authResponse := map[string]interface{}{
					"id":            msg["id"].(string),
					"origin_action": "AUTH",
					"result": map[string]interface{}{
						"browser_id":  deviceID,
						"user_id":     userID,
						"user_agent":  CustomHeaders.Get("User-Agent"),
						"timestamp":   time.Now().Unix(),
						"device_type": "extension",
						"version":     "2.5.0",
					},
				}
				err := c.WriteJSON(authResponse)
				if err != nil {
					logger.Error().Err(err).Msg("Error sending auth response")
					return
				}
				logger.Info().Str("ip", proxyIP).Interface("response", authResponse).Msg("Sent auth response")

			case "PONG":
				pongResponse := map[string]interface{}{
					"id":            msg["id"].(string),
					"origin_action": "PONG",
				}
				err := c.WriteJSON(pongResponse)
				if err != nil {
					logger.Error().Err(err).Msg("Error sending pong response")
					return
				}
				logger.Info().Str("ip", proxyIP).Interface("response", pongResponse).Msg("Sent pong response")
			}
		}
	}
}

func connectToWSS(ctx context.Context, socks5Proxy string, userID string, logger zerolog.Logger) {
	for {
		proxyURL := "socks5://" + socks5Proxy

		proxyParsed, err := url.Parse(proxyURL)
		if err != nil {
			logger.Error().Err(err).Msg("Error parsing proxy URL")
			time.Sleep(retryInterval)
			continue
		}

		deviceID := uuid.NewSHA1(uuid.NameSpaceDNS, []byte(proxyParsed.Host)).String()
		logger.Info().Str("deviceID", deviceID).Msg("Device ID")

		proxyIP, err := getProxyIP(socks5Proxy)
		if err != nil {
			logger.Error().Err(err).Msg("Error getting proxy IP")
			time.Sleep(retryInterval)
			continue
		}
		logger.Info().Str("proxyIP", proxyIP).Msg("Proxy IP")

		u := url.URL{Scheme: "wss", Host: "proxy.wynd.network:4650", Path: "/"}
		logger.Info().Str("url", u.String()).Msg("Connecting to")

		dialer := websocket.Dialer{
			Proxy:            http.ProxyURL(proxyParsed),
			TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
			HandshakeTimeout: 30 * time.Second,
		}

		c, _, err := dialer.DialContext(ctx, u.String(), CustomHeaders)
		if err != nil {
			logger.Error().Err(err).Msg("Error connecting to WebSocket")
			time.Sleep(retryInterval)
			continue
		}

		logger.Info().Msg("Connected to WebSocket")

		go sendPing(ctx, c, proxyIP, logger)
		receiveMessages(ctx, c, proxyIP, deviceID, userID, logger)

		c.Close()
		logger.Warn().Msg("Disconnected from WebSocket, retrying...")
		time.Sleep(retryInterval)
	}
}

func readProxies(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		proxies = append(proxies, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning file: %v", err)
	}

	return proxies, nil
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	fmt.Print("Input your user id: ")
	reader := bufio.NewReader(os.Stdin)
	userId, err := reader.ReadString('\n')
	if err != nil {
		log.Fatal().Err(err).Msg("Error reading user id")
	}
	userId = userId[:len(userId)-1]

	proxyFile := "proxy.txt"
	proxies, err := readProxies(proxyFile)
	if err != nil {
		log.Fatal().Err(err).Msg("Error reading proxies")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for _, proxy := range proxies {
		wg.Add(1)
		go func(proxy string) {
			defer wg.Done()
			connectToWSS(ctx, proxy, userId, log.Logger)
		}(proxy)
	}
	wg.Wait()
}
