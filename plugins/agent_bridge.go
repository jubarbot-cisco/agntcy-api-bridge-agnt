// Copyright AGNTCY Contributors (https://github.com/agntcy)
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/TykTechnologies/tyk/ctx"
	"github.com/TykTechnologies/tyk/log"
	"github.com/TykTechnologies/tyk/user"

	"github.com/TykTechnologies/kin-openapi/routers"
)

const (
	CONTENT_TYPE_NLQ          = "application/nlq"
	HEADER_X_NL_QUERY_ENABLED = "X-Nl-Query-Enabled"
	HEADER_X_NL_CONFIG        = "X-Nl-Config"

	INTERNAL_ERROR_MSG = "I'm sorry, but I wasn't able to process your request, it's an internal error"
	NO_SERVICE_FOUND   = "No service available to answer the request"
)

const (
	DEFAULT_RELEVANCE_THRESHOLD = 0.5
	DEFAULT_LLM_SEED            = 42
	DEFAULT_LLM_TEMPERATURE     = 0.0
)

const (
	METADATA_NLQ = "NLQuery"
)

var logger = log.Get()

func APIBridgeAgent(rw http.ResponseWriter, r *http.Request) {
	logger.Debugf("[+] Entering main entry point APIBridgeAgent")

	router := mux.NewRouter()

	router.HandleFunc("/api-bridge-agent/mcp/init", mcpInit).Methods(http.MethodPost)
	router.HandleFunc("/api-bridge-agent/mcp", processMCP).Methods(http.MethodPost).Headers("Content-Type", CONTENT_TYPE_NLQ)
	router.HandleFunc("/api-bridge-agent/aba", processACP).Methods(http.MethodPost).Headers("Content-Type", CONTENT_TYPE_NLQ)

	// Per API configuration
	router.PathPrefix("/").HandlerFunc(processPluginConfig).Methods(http.MethodDelete, http.MethodPut).Headers(HEADER_X_NL_CONFIG, "")

	// Catchall to real APIs
	router.PathPrefix("/").HandlerFunc(selectAndRewrite).Headers(HEADER_X_NL_QUERY_ENABLED, "")
	router.PathPrefix("/").HandlerFunc(selectAndRewrite).Methods(http.MethodPost).Headers("Content-Type", CONTENT_TYPE_NLQ)

	var match mux.RouteMatch
	var handler http.Handler
	if router.Match(r, &match) {
		handler = match.Handler
	}

	if handler == nil {
		return
	}

	handler.ServeHTTP(rw, r)
}

func APIBridgeAgentResponse(rw http.ResponseWriter, res *http.Response, req *http.Request) {
	RewriteResponseToNl(rw, res, req)
}

func processPluginConfig(rw http.ResponseWriter, r *http.Request) {
	apiConfig, err := getPluginFromRequest(r)
	if err != nil {
		logger.Debugf("[+] Failed to init plugin from request: %s", err)
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return
	}
	// Check if request is for configuration
	_, exists := r.Header[HEADER_X_NL_CONFIG]
	if exists {
		// implement the delete API (for cross API semantic routing support)
		if r.Method == "DELETE" {
			logger.Debugf("[+] Delete API '%s' for cross API semantic routing support ...", apiConfig.APIID)
			deletePluginConfig(apiConfig.APIID)
		}
		// implement the update API (for cross API semantic routing support)
		if r.Method == "PUT" {
			logger.Debugf("[+] Update API '%s' for cross API semantic routing support ...", apiConfig.APIID)
			if err := updatePluginConfig(apiConfig.APIID, r); err != nil {
				logger.Errorf("[+] Error while updating the plugin config: %s", err)
				http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
				return
			}
		}
		rw.WriteHeader(http.StatusOK)
		return
	}
}

func selectAndRewrite(rw http.ResponseWriter, r *http.Request) {
	logger.Debugf("[+] Inside selectAndRewrite ...")
	apiConfig, err := getPluginFromRequest(r)
	if err != nil {
		logger.Debugf("[+] Failed to init plugin from request: %s", err)
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return
	}

	if apiConfig.MaxRequestLength > 0 && r.ContentLength > apiConfig.MaxRequestLength {
		logger.Debugf("[+] Query is too large, ignoring ...")
		http.Error(rw, "Query is too large", http.StatusRequestEntityTooLarge)
		return
	}

	nlqBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Errorf("[+] Error while reading the body: %s", err)
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return
	}
	nlq := string(nlqBytes)

	session := &user.SessionState{
		MetaData: map[string]any{
			METADATA_NLQ: string(nlq),
		},
	}
	ctx.SetSession(r, session, true)

	if r.Header.Get(HEADER_X_NL_QUERY_ENABLED) != "" {
		err = rewriteQuery(r)
		if err != nil {
			logger.Errorf("[+] Error rewriting the query: %s", err)
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		return
	}

	matchingOperation, matchingScore, err := findSelectOperation(apiConfig.APIID, nlq)
	if err != nil {
		logger.Errorf("[+] Error while selecting operation: %s", err)
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return

	} else if matchingOperation == nil || matchingScore < apiConfig.RelevanceThreshold {
		logger.Debugf("[+] No matching operation found")
		http.Error(rw, "No matching operation found", http.StatusNotFound)
		return
	}
	logger.Debugf("[+] Selected endpoint: %#v - %#v", *matchingOperation, matchingScore)

	apidef := getOASDefinition(r)
	if apidef == nil {
		err := fmt.Errorf("API definition is nil")
		logger.Errorf("[+] selectAndRewrite: %s", err)
		return
	}

	// Iterate through all paths and operations in the API definition
	for path, pathItem := range apidef.Paths {
		for method, operation := range pathItem.Operations() {
			if operation.OperationID == *matchingOperation {
				route := &routers.Route{
					Path:      path,
					Method:    method,
					Operation: operation,
				}
				emptyPathParams := map[string]string{}
				r.URL.Path = path
				r.Method = method
				err := rewriteQueryForRoute(r, route, emptyPathParams)
				if err != nil {
					logger.Errorf("[+] Error rewriting the query: %s", err)
					http.Error(rw, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
	}
}

func RewriteResponseToNl(rw http.ResponseWriter, res *http.Response, req *http.Request) {
	_, err := getPluginFromRequest(req)
	if err != nil {
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return
	}

	session := ctx.GetSession(req)
	if session == nil {
		return
	}
	if _, present := session.MetaData[METADATA_NLQ]; !present {
		return
	}

	logger.Debug("[+] Rewriting response to Natural language ...")

	bodyBytes, err := io.ReadAll(res.Body)
	if err != nil {
		logger.Errorf("[+] Error while reading response body: %s", err)
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return
	}

	if res.Header.Get("Content-Encoding") == "gzip" {
		bodyBytes, err = GetUnzipContent(bodyBytes)
		if err != nil {
			logger.Errorf("[+] Error while unzipping the response body: %s", err)
			http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
			return
		}
		res.Header.Del("Content-Encoding")
	}

	naturalLanguageResponse, err := responseToNL(req, fmt.Sprintf("%s %s", res.Status, string(bodyBytes)))
	if err != nil {
		logger.Errorf("[+] Error while converting the response to Natural Language: %s", err)
		http.Error(rw, INTERNAL_ERROR_MSG, http.StatusInternalServerError)
		return
	}

	res.StatusCode = http.StatusOK

	res.Header.Set("Content-Type", "text/plain; charset=utf-8")
	res.Header.Set("Content-Length", fmt.Sprint(len(naturalLanguageResponse)))

	res.Body = io.NopCloser(strings.NewReader(naturalLanguageResponse))
	res.ContentLength = int64(len(naturalLanguageResponse))
}

func init() {
	logger.Infof("[+] Initializing API Bridge Agnt plugin (APIs)...")

	// Init Redis store, if needed
	if agentBridgeStore == nil {
		agentBridgeStore = getStorageForPlugin(context.TODO())
	}
}

func main() {}
