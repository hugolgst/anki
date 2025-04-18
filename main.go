package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type AnkiConnectRequest struct {
	Action  string                 `json:"action"`
	Version int                    `json:"version"`
	Params  map[string]interface{} `json:"params"`
}

type AnkiConnectResponse struct {
	Result interface{} `json:"result"`
	Error  *string     `json:"error"`
}

type CardStatus string

const (
	StatusNew        CardStatus = "new"
	StatusLearning   CardStatus = "learning"
	StatusReview     CardStatus = "review"
	StatusRelearning CardStatus = "relearning"
)

type Card struct {
	Word   string
	Status CardStatus
}

var tomlFile *string

func init() {
	defaultPath := "anki_stats.toml"
	homeDir, err := os.UserHomeDir()
	if err == nil {
		defaultPath = filepath.Join(homeDir, ".config", "anki-stats", "anki_stats.toml")
	} else {
		log.Printf("Warning: Could not determine home directory. Using default path in current directory: %s", defaultPath)
	}

	tomlFile = flag.String("o", defaultPath, "Path to the output TOML file")
	flag.Parse()
}

func invokeAnkiConnect(action string, params map[string]interface{}) (interface{}, error) {
	if params == nil {
		params = map[string]interface{}{}
	}

	request := AnkiConnectRequest{
		Action:  action,
		Version: 6,
		Params:  params,
	}

	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post("http://localhost:8765", "application/json", bytes.NewBuffer(requestJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to send request to AnkiConnect (is Anki running with AnkiConnect on http://localhost:8765?): %w", err)
	}
	defer resp.Body.Close()

	var response AnkiConnectResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if response.Error != nil {
		return nil, fmt.Errorf("AnkiConnect error: %s", *response.Error)
	}

	return response.Result, nil
}

func getCardInfo(cardID interface{}) (Card, error) {
	var card Card

	result, err := invokeAnkiConnect("cardsInfo", map[string]interface{}{
		"cards": []interface{}{cardID},
	})
	if err != nil {
		return card, err
	}

	cards, ok := result.([]interface{})
	if !ok || len(cards) == 0 {
		return card, fmt.Errorf("unexpected response format for card info: %T", result)
	}

	cardInfo, ok := cards[0].(map[string]interface{})
	if !ok {
		return card, fmt.Errorf("unexpected card info format: %T", cards[0])
	}

	fields, ok := cardInfo["fields"].(map[string]interface{})
	if !ok {
		return card, fmt.Errorf("could not get card fields map from card info")
	}

	wordValue := getFieldValue(fields, "Word")
	if wordValue == "" {
		log.Printf("Warning: 'Word' field is empty or missing for card ID %v. Trying other fields.", cardID)
		for fieldName, _ := range fields {
			value := getFieldValue(fields, fieldName)
			if value != "" {
				log.Printf("Using field '%s' with value '%s' as fallback.", fieldName, value)
				wordValue = value
				break
			}
		}
		if wordValue == "" {
			log.Printf("Error: Could not find any non-empty field for card ID %v. Skipping.", cardID)
			return card, fmt.Errorf("no usable field found for card ID %v", cardID)
		}
	}

	card.Word = wordValue
	card.Status = getCardStatus(cardInfo)

	return card, nil
}

func getFieldValue(fields map[string]interface{}, fieldName string) string {
	if field, ok := fields[fieldName]; ok {
		if fieldMap, ok := field.(map[string]interface{}); ok {
			if value, ok := fieldMap["value"].(string); ok {
				return value
			}
		}
	}
	return ""
}

func getCardStatus(cardInfo map[string]interface{}) CardStatus {
	queue, ok := cardInfo["queue"].(float64)
	if !ok {
		ctype, okType := cardInfo["type"].(float64)
		if okType {
			queue = ctype
		} else {
			log.Printf("Warning: Could not determine queue/type for card. Defaulting to 'review'. CardInfo: %v", cardInfo)
			return StatusReview
		}
	}

	switch int(queue) {
	case 0:
		return StatusNew
	case 1:
		return StatusLearning
	case 2:
		return StatusReview
	case 3:
		return StatusRelearning
	default:
		log.Printf("Warning: Unhandled card queue %d. Defaulting to 'review'.", int(queue))
		return StatusReview
	}
}

func appendToTOML(filename string, cardMap map[string]CardStatus) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", dir, err)
	}

	existing, err := os.ReadFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read %q: %w", filename, err)
	}

	today := time.Now().Format("2006-01-02")
	dateHeader := fmt.Sprintf("[%s]", today)

	var section strings.Builder
	section.WriteString(dateHeader + "\n")
	for word, status := range cardMap {
		escaped := strings.ReplaceAll(word, `"`, `\"`)
		section.WriteString(fmt.Sprintf("\"%s\" = \"%s\"\n", escaped, status))
	}
	newBlock := []byte(section.String())

	start := bytes.Index(existing, []byte(dateHeader))
	if start != -1 {
		searchFrom := start + len(dateHeader)
		next := bytes.Index(existing[searchFrom:], []byte("\n["))
		var end int
		if next == -1 {
			end = len(existing)
		} else {
			end = searchFrom + next + 1
		}
		existing = append(existing[:start], existing[end:]...)
		existing = bytes.TrimRight(existing, "\n")
	}

	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		existing = append(existing, '\n')
	}
	existing = append(existing, newBlock...)

	data := append(bytes.TrimRight(existing, "\n"), '\n')
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		return fmt.Errorf("failed to write %q: %w", filename, err)
	}
	return nil
}

func gitAddCommit(filePath string, numReviews int, numNewWords int) error {
	_, err := exec.Command("git", "add", filePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to add file to Git: %w", err)
	}

	now := time.Now()
	dateStr := now.Format("2006-01-02")
	commitMessage := fmt.Sprintf("Anki stats for %s: %d reviews, %d new words", dateStr, numReviews, numNewWords)

	_, err = exec.Command("git", "commit", "-m", commitMessage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to commit changes to Git: %w", err)
	}

	fmt.Printf("Successfully added and committed changes to Git with message: \"%s\"\n", commitMessage)
	return nil
}

func main() {
	outputFilePath := *tomlFile
	if outputFilePath == "" {
		log.Fatal("Output file path cannot be empty. Please specify with -o.")
	}

	versionResult, err := invokeAnkiConnect("version", nil)
	if err != nil {
		log.Fatalf("Failed to connect to AnkiConnect: %v", err)
	}
	versionStr := "unknown"
	if versionMap, ok := versionResult.(map[string]interface{}); ok {
		if v, ok := versionMap["version"].(string); ok {
			versionStr = v
		}
	} else if v, ok := versionResult.(float64); ok {
		versionStr = fmt.Sprintf("%.0f", v)
	} else if v, ok := versionResult.(string); ok {
		versionStr = v
	}

	fmt.Printf("Connected to AnkiConnect v%s\n", versionStr)

	uniqueCards := make(map[string]CardStatus)

	query := "rated:1"
	fmt.Printf("Querying Anki for cards matching: \"%s\"\n", query)
	reviewedResult, err := invokeAnkiConnect("findCards", map[string]interface{}{
		"query": query,
	})
	if err != nil {
		log.Fatalf("Failed to get reviewed cards: %v", err)
	}

	reviewedCardIDs, ok := reviewedResult.([]interface{})
	if !ok {
		log.Fatalf("Unexpected response format for findCards result: %T", reviewedResult)
	}

	fmt.Printf("Found %d card IDs potentially reviewed today\n", len(reviewedCardIDs))

	processedCount := 0
	skippedCount := 0
	newWordCount := 0
	for _, cardID := range reviewedCardIDs {
		card, err := getCardInfo(cardID)
		if err != nil {
			log.Printf("Warning: Error getting info for card ID %v: %v. Skipping card.", cardID, err)
			skippedCount++
			continue
		}

		if card.Word != "" {
			_, alreadySeen := uniqueCards[card.Word]
			uniqueCards[card.Word] = card.Status
			if !alreadySeen && card.Status == StatusNew {
				newWordCount++
			}
			processedCount++
		} else {
			log.Printf("Warning: Card ID %v resulted in an empty Word field after processing. Skipping.", cardID)
			skippedCount++
		}
	}

	fmt.Printf("Processed %d cards, skipped %d due to errors or empty word field.\n", processedCount, skippedCount)

	if len(uniqueCards) > 0 {
		if err := appendToTOML(outputFilePath, uniqueCards); err != nil {
			log.Fatalf("Failed to write to TOML file '%s': %v", outputFilePath, err)
		}

		fmt.Printf("\nSuccessfully logged %d unique cards (%d new) to %s\n", len(uniqueCards), newWordCount, outputFilePath)

		fmt.Println("\nSample of logged cards:")
		i := 0
		for word, status := range uniqueCards {
			if i >= 5 {
				fmt.Printf("... and %d more\n", len(uniqueCards)-5)
				break
			}
			printableWord := strings.ReplaceAll(word, "\n", " ")
			fmt.Printf("- \"%s\" = \"%s\"\n", printableWord, status)
			i++
		}

		if err := gitAddCommit(outputFilePath, len(uniqueCards), newWordCount); err != nil {
			log.Printf("Warning: Failed to add and commit changes to Git: %v", err)
		}
	} else {
		fmt.Println("No unique cards with non-empty words found to log today.")
	}
}
