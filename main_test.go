// main_test.go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Unit Test
func TestGenerateOTP(t *testing.T) {
	fmt.Println("Running Unit Test...")
	otp := GenerateOTP()
	assert.Equal(t, 6, len(otp), "OTP should have length of 6")
	fmt.Println("TestGenerateOTP: Success")
}

// Integration Test
func TestFetchNewsFromAPI(t *testing.T) {
	fmt.Println("Running Integration Test...")
	apiKey := "84b7be9be9f746c8a5a08894ea376461"
	keyword := "fashion"
	newsList, err := fetchNewsFromAPI(apiKey, keyword)
	assert.NoError(t, err, "Fetching news from API should not return error")
	assert.NotEmpty(t, newsList, "News list should not be empty")
	fmt.Println("TestFetchNewsFromAPI: Success")
}

// End-To-End Test
func TestIndexHandler(t *testing.T) {
	fmt.Println("Running End-To-End Test...")

	req, err := http.NewRequest("GET", "/register", nil)
	assert.NoError(t, err, "Creating request should not return error")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(RegisterHandler)

	handler.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		fmt.Println("TestIndexHandler: Success")
	} else {
		fmt.Println("TestIndexHandler: Fail")
	}

	assert.Equal(t, http.StatusOK, rr.Code, "HTTP status should be OK")
}
