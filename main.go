 package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
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
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory '%s': %w", dir, err)
	}

	var contentBytes []byte
	contentBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("File '%s' not found, will create a new one.", filename)
			contentBytes = []byte{}
		} else {
			return fmt.Errorf("failed to read file '%s': %w", filename, err)
		}
	}
	content := string(contentBytes)

	today := time.Now().Format("2006-01-02")
	dateHeader := fmt.Sprintf("[%s]", today)

	var newContent strings.Builder

	if strings.Contains(content, dateHeader) {
		lines := strings.Split(content, "\n")
		inTodaySection := false
		addedNewCards := false

		for i, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			isLastLine := i == len(lines)-1

			if trimmedLine == dateHeader {
				inTodaySection = true
				newContent.WriteString(line + "\n")

				for word, status := range cardMap {
					escapedWord := strings.ReplaceAll(word, "\"", "\\\"")
					newContent.WriteString(fmt.Sprintf("\"%s\" = \"%s\"\n", escapedWord, status))
				}
				addedNewCards = true

			} else if inTodaySection && (strings.HasPrefix(trimmedLine, "[") && strings.HasSuffix(trimmedLine, "]")) {
				inTodaySection = false
				newContent.WriteString(line)
				if !isLastLine {
					newContent.WriteString("\n")
				}
			} else if !inTodaySection {
				newContent.WriteString(line)
				if !isLastLine {
					newContent.WriteString("\n")
				}
			}
		}

		if !addedNewCards && inTodaySection {
			for word, status := range cardMap {
				escapedWord := strings.ReplaceAll(word, "\"", "\\\"")
				newContent.WriteString(fmt.Sprintf("\"%s\" = \"%s\"\n", escapedWord, status))
			}
		}
		content = newContent.String()

	} else {
		var newSection strings.Builder
		if len(strings.TrimSpace(content)) > 0 {
			newSection.WriteString("\n")
		}
		newSection.WriteString(dateHeader + "\n")

		for word, status := range cardMap {
			escapedWord := strings.ReplaceAll(word, "\"", "\\\"")
			newSection.WriteString(fmt.Sprintf("\"%s\" = \"%s\"\n", escapedWord, status))
		}

		content += newSection.String()
	}

	return ioutil.WriteFile(filename, []byte(strings.TrimRight(content, "\n")+"\n"), 0644)
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
	for _, cardID := range reviewedCardIDs {
		card, err := getCardInfo(cardID)
		if err != nil {
			log.Printf("Warning: Error getting info for card ID %v: %v. Skipping card.", cardID, err)
			skippedCount++
			continue
		}

		if card.Word != "" {
			uniqueCards[card.Word] = card.Status
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

		fmt.Printf("\nSuccessfully logged %d unique cards to %s\n", len(uniqueCards), outputFilePath)

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
	} else {
		fmt.Println("No unique cards with non-empty words found to log today.")
	}
}
