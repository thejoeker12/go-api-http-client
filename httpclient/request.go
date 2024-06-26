// http_request.go
package httpclient

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/deploymenttheory/go-api-http-client/authenticationhandler"
	"github.com/deploymenttheory/go-api-http-client/headers"
	"github.com/deploymenttheory/go-api-http-client/httpmethod"
	"github.com/deploymenttheory/go-api-http-client/logger"
	"github.com/deploymenttheory/go-api-http-client/ratehandler"
	"github.com/deploymenttheory/go-api-http-client/response"
	"github.com/deploymenttheory/go-api-http-client/status"
	"go.uber.org/zap"
)

// DoRequest constructs and executes an HTTP request based on the provided method, endpoint, request body, and output variable.
// This function serves as a dispatcher, deciding whether to execute the request with or without retry logic based on the
// idempotency of the HTTP method. Idempotent methods (GET, PUT, DELETE) are executed with retries to handle transient errors
// and rate limits, while non-idempotent methods (POST, PATCH) are executed without retries to avoid potential side effects
// of duplicating non-idempotent operations. The function uses an instance of a logger implementing the logger.Logger interface,
// used to log informational messages, warnings, and errors encountered during the execution of the request.
// It also applies redirect handling to the client if configured, allowing the client to follow redirects up to a maximum
// number of times.

// Parameters:
// - method: A string representing the HTTP method to be used for the request. This method determines the execution path
//   and whether the request will be retried in case of failures.
// - endpoint: The target API endpoint for the request. This should be a relative path that will be appended to the base URL
//   configured for the HTTP client.
// - body: The payload for the request, which will be serialized into the request body. The serialization format (e.g., JSON, XML)
//   is determined by the content-type header and the specific implementation of the API handler used by the client.
// - out: A pointer to an output variable where the response will be deserialized. The function expects this to be a pointer to
//   a struct that matches the expected response schema.

// Returns:
// - *http.Response: The HTTP response received from the server. In case of successful execution, this response contains
//   the status code, headers, and body of the response. In case of errors, particularly after exhausting retries for
//   idempotent methods, this response may contain the last received HTTP response that led to the failure.
// - error: An error object indicating failure during request execution. This could be due to network issues, server errors,
//   or a failure in request serialization/deserialization. For idempotent methods, an error is returned if all retries are
//   exhausted without success.

// Usage:
// This function is the primary entry point for executing HTTP requests using the client. It abstracts away the details of
// request retries, serialization, and response handling, providing a simplified interface for making HTTP requests. It is
// suitable for a wide range of HTTP operations, from fetching data with GET requests to submitting data with POST requests.

// Example:
// var result MyResponseType
// resp, err := client.DoRequest("GET", "/api/resource", nil, &result, logger)
// if err != nil {
//     // Handle error
// }
// // Use `result` or `resp` as needed

// Note:
// - The caller is responsible for closing the response body when not nil to avoid resource leaks.
// - The function ensures concurrency control by managing concurrency tokens internally, providing safe concurrent operations
//   within the client's concurrency model.
// - The decision to retry requests is based on the idempotency of the HTTP method and the client's retry configuration,
//   including maximum retry attempts and total retry duration.

func (c *Client) DoRequest(method, endpoint string, body, out interface{}) (*http.Response, error) {
	log := c.Logger

	if httpmethod.IsIdempotentHTTPMethod(method) {
		return c.executeRequestWithRetries(method, endpoint, body, out)
	} else if httpmethod.IsNonIdempotentHTTPMethod(method) {
		return c.executeRequest(method, endpoint, body, out)
	} else {
		return nil, log.Error("HTTP method not supported", zap.String("method", method))
	}
}

// executeRequestWithRetries executes an HTTP request using the specified method, endpoint, request body, and output variable.
// It is designed for idempotent HTTP methods (GET, PUT, DELETE), where the request can be safely retried in case of
// transient errors or rate limiting. The function implements a retry mechanism that respects the client's configuration
// for maximum retry attempts and total retry duration. Each retry attempt uses exponential backoff with jitter to avoid
// thundering herd problems. An instance of a logger (conforming to the logger.Logger interface) is used for logging the
// request, retry attempts, and any errors encountered.
//
// Parameters:
// - method: The HTTP method to be used for the request (e.g., "GET", "PUT", "DELETE").
// - endpoint: The API endpoint to which the request will be sent. This should be a relative path that will be appended
// to the base URL of the HTTP client.
// - body: The request payload, which will be marshaled into the request body based on the content type. Can be nil for
// methods that do not send a payload.
// - out: A pointer to the variable where the unmarshaled response will be stored. The function expects this to be a
// pointer to a struct that matches the expected response schema.
// - log:
//
// Returns:
// - *http.Response: The HTTP response from the server, which may be the response from a successful request or the last
// failed attempt if all retries are exhausted.
//   - error: An error object if an error occurred during the request execution or if all retry attempts failed. The error
//     may be a structured API error parsed from the response or a generic error indicating the failure reason.
//
// Usage:
// This function should be used for operations that are safe to retry and where the client can tolerate the additional
// latency introduced by the retry mechanism. It is particularly useful for handling transient errors and rate limiting
// responses from the server.
//
// Note:
// - The caller is responsible for closing the response body to prevent resource leaks.
// - The function respects the client's concurrency token, acquiring and releasing it as needed to ensure safe concurrent
// operations.
// - The retry mechanism employs exponential backoff with jitter to mitigate the impact of retries on the server.
func (c *Client) executeRequestWithRetries(method, endpoint string, body, out interface{}) (*http.Response, error) {
	log := c.Logger

	// Include the core logic for handling non-idempotent requests with retries here.
	log.Debug("Executing request with retries", zap.String("method", method), zap.String("endpoint", endpoint))

	// Auth Token validation check
	clientCredentials := authenticationhandler.ClientCredentials{
		Username:     c.clientConfig.Auth.Username,
		Password:     c.clientConfig.Auth.Password,
		ClientID:     c.clientConfig.Auth.ClientID,
		ClientSecret: c.clientConfig.Auth.ClientSecret,
	}

	valid, err := c.AuthTokenHandler.CheckAndRefreshAuthToken(c.APIHandler, c.httpClient, clientCredentials, c.clientConfig.ClientOptions.Timeout.TokenRefreshBufferPeriod)
	if err != nil || !valid {
		return nil, err
	}

	// Acquire a concurrency permit along with a unique request ID
	ctx, requestID, err := c.ConcurrencyHandler.AcquireConcurrencyPermit(context.Background())
	if err != nil {
		return nil, c.Logger.Error("Failed to acquire concurrency permit", zap.Error(err))
	}

	// Ensure the permit is released after the function exits
	defer func() {
		c.ConcurrencyHandler.ReleaseConcurrencyPermit(requestID)
	}()

	// Marshal Request with correct encoding defined in api handler
	requestData, err := c.APIHandler.MarshalRequest(body, method, endpoint, log)
	if err != nil {
		return nil, err
	}

	// Construct URL with correct structure defined in api handler
	url := c.APIHandler.ConstructAPIResourceEndpoint(endpoint, log)

	// Increment total request counter within ConcurrencyHandler's metrics
	c.ConcurrencyHandler.Metrics.Lock.Lock()
	c.ConcurrencyHandler.Metrics.TotalRequests++
	c.ConcurrencyHandler.Metrics.Lock.Unlock()

	// Create a new HTTP request with the provided method, URL, and body
	req, err := http.NewRequest(method, url, bytes.NewBuffer(requestData))
	if err != nil {
		return nil, err
	}

	// Apply custom cookies if configured
	// cookiejar.ApplyCustomCookies(req, c.clientConfig.ClientOptions.Cookies.CustomCookies, log)

	// Set request headers
	headerHandler := headers.NewHeaderHandler(req, c.Logger, c.APIHandler, c.AuthTokenHandler)
	headerHandler.SetRequestHeaders(endpoint)
	headerHandler.LogHeaders(c.clientConfig.ClientOptions.Logging.HideSensitiveData)

	// Define a retry deadline based on the client's total retry duration configuration
	totalRetryDeadline := time.Now().Add(c.clientConfig.ClientOptions.Timeout.TotalRetryDuration)

	var resp *http.Response
	var retryCount int
	for time.Now().Before(totalRetryDeadline) { // Check if the current time is before the total retry deadline
		req = req.WithContext(ctx)

		// Log outgoing cookies
		log.LogCookies("outgoing", req, method, endpoint)

		// Execute the HTTP request
		resp, err = c.do(req, log, method, endpoint)

		// Log outgoing cookies
		log.LogCookies("incoming", req, method, endpoint)

		// Check for successful status code
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 400 {
			if resp.StatusCode >= 300 {
				log.Warn("Redirect response received", zap.Int("status_code", resp.StatusCode), zap.String("location", resp.Header.Get("Location")))
			}
			// Handle the response as successful.
			return resp, response.HandleAPISuccessResponse(resp, out, log)
		}

		// Leverage TranslateStatusCode for more descriptive error logging
		statusMessage := status.TranslateStatusCode(resp)

		// Check for non-retryable errors
		if resp != nil && status.IsNonRetryableStatusCode(resp) {
			log.Warn("Non-retryable error received", zap.Int("status_code", resp.StatusCode), zap.String("status_message", statusMessage))
			return resp, response.HandleAPIErrorResponse(resp, log)
		}

		// Parsing rate limit headers if a rate-limit error is detected
		if status.IsRateLimitError(resp) {
			waitDuration := ratehandler.ParseRateLimitHeaders(resp, log)
			if waitDuration > 0 {
				log.Warn("Rate limit encountered, waiting before retrying", zap.Duration("waitDuration", waitDuration))
				time.Sleep(waitDuration)
				continue // Continue to next iteration after waiting
			}
		}

		// Handling retryable errors with exponential backoff
		if status.IsTransientError(resp) {
			retryCount++
			if retryCount > c.clientConfig.ClientOptions.Retry.MaxRetryAttempts {
				log.Warn("Max retry attempts reached", zap.String("method", method), zap.String("endpoint", endpoint))
				break // Stop retrying if max attempts are reached
			}
			waitDuration := ratehandler.CalculateBackoff(retryCount)
			log.Warn("Retrying request due to transient error", zap.String("method", method), zap.String("endpoint", endpoint), zap.Int("retryCount", retryCount), zap.Duration("waitDuration", waitDuration), zap.Error(err))
			time.Sleep(waitDuration) // Wait before retrying
			continue                 // Continue to next iteration after waiting
		}

		// Handle error responses
		if err != nil || !status.IsRetryableStatusCode(resp.StatusCode) {
			if apiErr := response.HandleAPIErrorResponse(resp, log); apiErr != nil {
				err = apiErr
			}
			log.LogError("request_error", method, endpoint, resp.StatusCode, resp.Status, err, status.TranslateStatusCode(resp))
			break
		}
	}

	// Handles final non-API error.
	if err != nil {
		return nil, err
	}

	return resp, response.HandleAPIErrorResponse(resp, log)
}

// executeRequest executes an HTTP request using the specified method, endpoint, and request body without implementing
// retry logic. It is primarily designed for non idempotent HTTP methods like POST and PATCH, where the request should
// not be automatically retried within this function due to the potential side effects of re-submitting the same data.
//
// Parameters:
// - method: The HTTP method to be used for the request, typically "POST" or "PATCH".
// - endpoint: The API endpoint to which the request will be sent. This should be a relative path that will be appended
// to the base URL of the HTTP client.
//   - body: The request payload, which will be marshaled into the request body based on the content type. This can be any
//     data structure that can be marshaled into the expected request format (e.g., JSON, XML).
//   - out: A pointer to the variable where the unmarshaled response will be stored. This should be a pointer to a struct
//
// that matches the expected response schema.
// - log: An instance of a logger (conforming to the logger.Logger interface) used for logging the request and any errors
// encountered.
//
// Returns:
// - *http.Response: The HTTP response from the server. This includes the status code, headers, and body of the response.
// - error: An error object if an error occurred during the request execution. This could be due to network issues,
// server errors, or issues with marshaling/unmarshaling the request/response.
//
// Usage:
// This function is suitable for operations where the request should not be retried automatically, such as data submission
// operations where retrying could result in duplicate data processing. It ensures that the request is executed exactly
// once and provides detailed logging for debugging purposes.
//
// Note:
// - The caller is responsible for closing the response body to prevent resource leaks.
// - The function ensures concurrency control by acquiring and releasing a concurrency token before and after the request
// execution.
// - The function logs detailed information about the request execution, including the method, endpoint, status code, and
// any errors encountered.
func (c *Client) executeRequest(method, endpoint string, body, out interface{}) (*http.Response, error) {
	log := c.Logger

	// Include the core logic for handling idempotent requests here.
	log.Debug("Executing request without retries", zap.String("method", method), zap.String("endpoint", endpoint))

	// Auth Token validation check
	clientCredentials := authenticationhandler.ClientCredentials{
		Username:     c.clientConfig.Auth.Username,
		Password:     c.clientConfig.Auth.Password,
		ClientID:     c.clientConfig.Auth.ClientID,
		ClientSecret: c.clientConfig.Auth.ClientSecret,
	}

	valid, err := c.AuthTokenHandler.CheckAndRefreshAuthToken(c.APIHandler, c.httpClient, clientCredentials, c.clientConfig.ClientOptions.Timeout.TokenRefreshBufferPeriod)
	if err != nil || !valid {
		return nil, err
	}

	// Acquire a concurrency permit along with a unique request ID
	ctx, requestID, err := c.ConcurrencyHandler.AcquireConcurrencyPermit(context.Background())
	if err != nil {
		return nil, c.Logger.Error("Failed to acquire concurrency permit", zap.Error(err))
	}

	// Ensure the permit is released after the function exits
	defer func() {
		c.ConcurrencyHandler.ReleaseConcurrencyPermit(requestID)
	}()

	// Determine which set of encoding and content-type request rules to use
	apiHandler := c.APIHandler

	// Marshal Request with correct encoding
	requestData, err := apiHandler.MarshalRequest(body, method, endpoint, log)
	if err != nil {
		return nil, err
	}

	// Construct URL using the ConstructAPIResourceEndpoint function
	url := c.APIHandler.ConstructAPIResourceEndpoint(endpoint, log)

	// Create a new HTTP request with the provided method, URL, and body
	req, err := http.NewRequest(method, url, bytes.NewBuffer(requestData))
	if err != nil {
		return nil, err
	}

	// Apply custom cookies if configured
	// cookiejar.ApplyCustomCookies(req, c.clientConfig.ClientOptions.Cookies.CustomCookies, log)

	// Set request headers
	headerHandler := headers.NewHeaderHandler(req, c.Logger, c.APIHandler, c.AuthTokenHandler)
	headerHandler.SetRequestHeaders(endpoint)
	headerHandler.LogHeaders(c.clientConfig.ClientOptions.Logging.HideSensitiveData)

	req = req.WithContext(ctx)

	// Log outgoing cookies
	log.LogCookies("outgoing", req, method, endpoint)

	// Measure the time taken to execute the request and receive the response
	startTime := time.Now()

	// Execute the HTTP request
	resp, err := c.do(req, log, method, endpoint)
	if err != nil {
		return nil, err
	}

	// Calculate the duration between sending the request and receiving the response
	duration := time.Since(startTime)

	// Evaluate and adjust concurrency based on the request's feedback
	c.ConcurrencyHandler.EvaluateAndAdjustConcurrency(resp, duration)

	// Log outgoing cookies
	log.LogCookies("incoming", req, method, endpoint)

	// Checks for the presence of a deprecation header in the HTTP response and logs if found.
	headers.CheckDeprecationHeader(resp, log)

	// Check for successful status code, including redirects
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		// Warn on redirects but proceed as successful
		if resp.StatusCode >= 300 {
			log.Warn("Redirect response received", zap.Int("status_code", resp.StatusCode), zap.String("location", resp.Header.Get("Location")))
		}
		return resp, response.HandleAPISuccessResponse(resp, out, log)

	}

	// Handle error responses for status codes outside the successful range
	return nil, response.HandleAPIErrorResponse(resp, log)
}

// do sends an HTTP request using the client's HTTP client. It logs the request and error details, if any,
// using structured logging with zap fields.
//
// Parameters:
// - req: The *http.Request object that contains all the details of the HTTP request to be sent.
// - log: An instance of a logger (conforming to the logger.Logger interface) used for logging the request details and any
// errors.
// - method: The HTTP method used for the request, used for logging.
// - endpoint: The API endpoint the request is being sent to, used for logging.
//
// Returns:
// - *http.Response: The HTTP response from the server.
// - error: An error object if an error occurred while sending the request or nil if no error occurred.
//
// Usage:
// This function should be used whenever the client needs to send an HTTP request. It abstracts away the common logic of
// request execution and error handling, providing detailed logs for debugging and monitoring.
func (c *Client) do(req *http.Request, log logger.Logger, method, endpoint string) (*http.Response, error) {

	resp, err := c.httpClient.Do(req)

	if err != nil {
		// Log the error with structured logging, including method, endpoint, and the error itself
		log.Error("Failed to send request", zap.String("method", method), zap.String("endpoint", endpoint), zap.Error(err))
		return nil, err
	}

	// Log the response status code for successful requests
	log.Debug("Request sent successfully", zap.String("method", method), zap.String("endpoint", endpoint), zap.Int("status_code", resp.StatusCode))

	return resp, nil
}
