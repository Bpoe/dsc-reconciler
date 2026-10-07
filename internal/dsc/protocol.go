package dsc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const protocolVersion = "2024-11-05"

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	id           uint64
	result       json.RawMessage
	rpcErr       *rpcError
	notification bool
	err          error
}

type rpcError struct {
	Code    int
	Message string
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("DSC JSON-RPC error %d: %s", e.Code, e.Message)
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > stdoutLimit {
			return nil, errors.New("DSC JSON-RPC frame exceeds 16 MiB")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if len(line) != 0 {
				return nil, errors.New("incomplete DSC JSON-RPC frame")
			}
			return nil, err
		}
		return line, nil
	}
}

func object(data []byte) bool {
	data = bytes.TrimSpace(data)
	return len(data) != 0 && data[0] == '{'
}

func decodeResponse(line []byte) (response, error) {
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  json.RawMessage `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if !utf8.Valid(line) || !object(line) || json.Unmarshal(line, &envelope) != nil || envelope.JSONRPC != "2.0" {
		return response{}, errors.New("invalid DSC JSON-RPC envelope")
	}
	if envelope.Method != nil {
		var method string
		if json.Unmarshal(envelope.Method, &method) != nil || method == "" ||
			envelope.ID != nil || envelope.Result != nil || envelope.Error != nil {
			return response{}, errors.New("unexpected DSC JSON-RPC request or malformed notification")
		}
		params := bytes.TrimSpace(envelope.Params)
		if len(params) != 0 && params[0] != '{' && params[0] != '[' {
			return response{}, errors.New("invalid DSC notification parameters")
		}
		return response{notification: true}, nil
	}
	var id uint64
	if envelope.ID == nil || json.Unmarshal(envelope.ID, &id) != nil || id == 0 ||
		(envelope.Result == nil) == (envelope.Error == nil) || envelope.Params != nil {
		return response{}, errors.New("invalid DSC JSON-RPC response shape")
	}
	if envelope.Error != nil {
		var failure struct {
			Code    *int    `json:"code"`
			Message *string `json:"message"`
		}
		if !object(envelope.Error) || json.Unmarshal(envelope.Error, &failure) != nil || failure.Code == nil || failure.Message == nil {
			return response{}, errors.New("invalid DSC JSON-RPC error")
		}
		return response{id: id, rpcErr: &rpcError{Code: *failure.Code, Message: *failure.Message}}, nil
	}
	if !object(envelope.Result) {
		return response{}, errors.New("DSC JSON-RPC result must be an object")
	}
	return response{id: id, result: envelope.Result}, nil
}

func validateInitialization(raw json.RawMessage) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ProtocolVersion != protocolVersion ||
		!object(result.Capabilities.Tools) || result.ServerInfo.Name == "" || result.ServerInfo.Version == "" {
		return errors.New("DSC initialize response has incompatible protocol or missing server/tool capabilities")
	}
	return nil
}

func parseToolResult(raw json.RawMessage) (json.RawMessage, bool, string, error) {
	var result struct {
		IsError           json.RawMessage `json:"isError"`
		StructuredContent struct {
			Result json.RawMessage `json:"result"`
		} `json:"structuredContent"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, false, "", errors.New("invalid DSC tool result shape")
	}
	isError := bytes.TrimSpace(result.IsError)
	if isError != nil && !bytes.Equal(isError, []byte("true")) && !bytes.Equal(isError, []byte("false")) {
		return nil, false, "", errors.New("invalid DSC tool isError flag")
	}
	if bytes.Equal(isError, []byte("true")) {
		messages := []string{"DSC tool reported an error"}
		for _, content := range result.Content {
			if content.Type == "text" {
				messages = append(messages, content.Text)
			}
		}
		return nil, true, strings.Join(messages, ": "), nil
	}
	payload, hadErrors, err := parseOutput(result.StructuredContent.Result)
	return payload, hadErrors, "", err
}

func parseOutput(data []byte) (json.RawMessage, bool, error) {
	var result map[string]json.RawMessage
	if !utf8.Valid(data) || !object(data) || json.Unmarshal(data, &result) != nil {
		return nil, false, errors.New("DSC structuredContent.result must be a JSON object")
	}
	for name, opening := range map[string]byte{"metadata": '{', "results": '[', "messages": '['} {
		value := bytes.TrimSpace(result[name])
		if len(value) == 0 || value[0] != opening {
			return nil, false, fmt.Errorf("DSC result has missing or invalid %s", name)
		}
	}
	raw := bytes.TrimSpace(result["hadErrors"])
	if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
		return nil, false, errors.New("DSC result has missing or invalid hadErrors")
	}
	return bytes.Clone(data), bytes.Equal(raw, []byte("true")), nil
}
