package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"

	"github.com/gin-gonic/gin"
)

type GeminiChatRequest struct {
	Contents         []GeminiChatContents       `json:"contents"`
	SafetySettings   []GeminiChatSafetySettings `json:"safety_settings"`
	GenerationConfig GeminiChatGenerationConfig `json:"generation_config"`
}
type GeminiChatParts struct {
	Text string `json:"text"`
}
type GeminiChatContents struct {
	Role  string          `json:"role"`
	Parts GeminiChatParts `json:"parts"`
}
type GeminiChatSafetySettings struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}
type GeminiChatGenerationConfig struct {
	Temperature     float64 `json:"temperature"`
	TopP            float64 `json:"topP"`
	TopK            int     `json:"topK"`
	MaxOutputTokens int     `json:"maxOutputTokens"`
}

// Setting safety to the lowest possible values since Gemini is already powerless enough
func requestOpenAI2GeminiChat(textRequest GeneralOpenAIRequest) *GeminiChatRequest {
	geminiRequest := GeminiChatRequest{
		Contents: make([]GeminiChatContents, 0, len(textRequest.Messages)),
		SafetySettings: []GeminiChatSafetySettings{
			{
				Category:  "HARM_CATEGORY_HARASSMENT",
				Threshold: "BLOCK_ONLY_HIGH",
			},
			{
				Category:  "HARM_CATEGORY_HATE_SPEECH",
				Threshold: "BLOCK_ONLY_HIGH",
			},
			{
				Category:  "HARM_CATEGORY_SEXUALLY_EXPLICIT",
				Threshold: "BLOCK_ONLY_HIGH",
			},
			{
				Category:  "HARM_CATEGORY_DANGEROUS_CONTENT",
				Threshold: "BLOCK_ONLY_HIGH",
			},
		},
		GenerationConfig: GeminiChatGenerationConfig{
			Temperature:     textRequest.Temperature,
			TopP:            textRequest.TopP,
			TopK:            textRequest.MaxTokens,
			MaxOutputTokens: textRequest.MaxTokens,
		},
	}
	for _, message := range textRequest.Messages {
		content := GeminiChatContents{
			Role: message.Role,
			Parts: GeminiChatParts{
				Text: message.StringContent(),
			},
		}
		geminiRequest.Contents = append(geminiRequest.Contents, content)
	}
	return &geminiRequest
}

type GeminiChatResponse struct {
	Candidates     []GeminiChatCandidate    `json:"candidates"`
	PromptFeedback GeminiChatPromptFeedback `json:"promptFeedback"`
}

type GeminiChatCandidate struct {
	Content       GeminiChatContent        `json:"content"`
	FinishReason  string                   `json:"finishReason"`
	Index         int64                    `json:"index"`
	SafetyRatings []GeminiChatSafetyRating `json:"safetyRatings"`
}

type GeminiChatContent struct {
	Parts []GeminiChatPart `json:"parts"`
	Role  string           `json:"role"`
}

type GeminiChatPart struct {
	Text string `json:"text"`
}

type GeminiChatSafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
}

type GeminiChatPromptFeedback struct {
	SafetyRatings []GeminiChatSafetyRating `json:"safetyRatings"`
}

func responseGeminiChat2OpenAI(response *GeminiChatResponse) *OpenAITextResponse {
	fullTextResponse := OpenAITextResponse{
		Choices: make([]OpenAITextResponseChoice, 0, len(response.Candidates)),
	}
	for i, candidate := range response.Candidates {
		choice := OpenAITextResponseChoice{
			Index: i,
			Message: Message{
				Role:    "assistant",
				Content: candidate.Content.Parts[0].Text,
			},
			FinishReason: "stop",
		}
		fullTextResponse.Choices = append(fullTextResponse.Choices, choice)
	}
	return &fullTextResponse
}

func streamResponseGeminiChat2OpenAI(geminiResponse *GeminiChatResponse) *ChatCompletionsStreamResponse {
	var choice ChatCompletionsStreamResponseChoice
	if len(geminiResponse.Candidates) > 0 {
		choice.Delta.Content = geminiResponse.Candidates[0].Content.Parts[0].Text
	}
	choice.FinishReason = &stopFinishReason
	var response ChatCompletionsStreamResponse
	response.Object = "chat.completion.chunk"
	response.Model = "gemini"
	response.Choices = []ChatCompletionsStreamResponseChoice{choice}
	return &response
}

func geminiChatStreamHandler(c *gin.Context, resp *http.Response) (*OpenAIErrorWithStatusCode, string) {
	responseText := ""
	responseId := fmt.Sprintf("chatcmpl-%s", common.GetUUID())
	createdTime := common.GetTimestamp()
	dataChan := make(chan string)
	stopChan := make(chan bool)
	go func() {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			common.SysError("error reading stream response: " + err.Error())
			stopChan <- true
			return
		}
		err = resp.Body.Close()
		if err != nil {
			common.SysError("error closing stream response: " + err.Error())
			stopChan <- true
			return
		}
		var geminiResponse GeminiChatResponse
		err = json.Unmarshal(responseBody, &geminiResponse)
		if err != nil {
			common.SysError("error unmarshalling stream response: " + err.Error())
			stopChan <- true
			return
		}
		fullTextResponse := streamResponseGeminiChat2OpenAI(&geminiResponse)
		fullTextResponse.Id = responseId
		fullTextResponse.Created = createdTime
		if len(geminiResponse.Candidates) > 0 {
			responseText = geminiResponse.Candidates[0].Content.Parts[0].Text
		}
		jsonResponse, err := json.Marshal(fullTextResponse)
		if err != nil {
			common.SysError("error marshalling stream response: " + err.Error())
			stopChan <- true
			return
		}
		dataChan <- string(jsonResponse)
		stopChan <- true
	}()
	setEventStreamHeaders(c)
	c.Stream(func(w io.Writer) bool {
		select {
		case data := <-dataChan:
			c.Render(-1, common.CustomEvent{Data: "data: " + data})
			return true
		case <-stopChan:
			c.Render(-1, common.CustomEvent{Data: "data: [DONE]"})
			return false
		}
	})
	err := resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), ""
	}
	return nil, responseText
}

func geminiChatHandler(c *gin.Context, resp *http.Response, promptTokens int, model string) (*OpenAIErrorWithStatusCode, *Usage) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	var geminiResponse GeminiChatResponse
	err = json.Unmarshal(responseBody, &geminiResponse)
	if err != nil {
		return errorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	if len(geminiResponse.Candidates) == 0 {
		return &OpenAIErrorWithStatusCode{
			OpenAIError: OpenAIError{
				Message: "No candidates returned",
				Type:    "server_error",
				Param:   "",
				Code:    500,
			},
			StatusCode: resp.StatusCode,
		}, nil
	}
	fullTextResponse := responseGeminiChat2OpenAI(&geminiResponse)
	completionTokens := countTokenText(geminiResponse.Candidates[0].Content.Parts[0].Text, model)
	usage := Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
	fullTextResponse.Usage = usage
	jsonResponse, err := json.Marshal(fullTextResponse)
	if err != nil {
		return errorWrapper(err, "marshal_response_body_failed", http.StatusInternalServerError), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return nil, &usage
}
