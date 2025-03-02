// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"net/http"
	"testing"

	"github.com/google/martian/v3"
)

func TestApiForwarder(t *testing.T) {
	forwarder := NewForwarder("", 8181)

	req, err := http.NewRequest("GET", "https://martian.proxy/configure", nil)
	if err != nil {
		t.Fatalf("NewRequest(): got %v, want no error", err)
	}

	ctx := martian.TestContext(req, nil, nil)

	if err := forwarder.ModifyRequest(req); err != nil {
		t.Fatalf("ModifyRequest(): got %v, want no error", err)
	}

	if got, want := req.URL.Scheme, "http"; got != want {
		t.Errorf("req.URL.Scheme: got %s, want %s", got, want)
	}
	if got, want := req.URL.Host, "localhost:8181"; got != want {
		t.Errorf("req.URL.Host: got %s, want %s", got, want)
	}

	if !ctx.SkippingLogging() {
		t.Errorf("ctx.SkippingLogging: got false, want true")
	}

	if !ctx.IsAPIRequest() {
		t.Errorf("ctx.IsApiRequest: got false, want true")
	}
}

func TestApiForwarderWithHost(t *testing.T) {
	forwarder := NewForwarder("example.com", 8181)

	req, err := http.NewRequest("GET", "https://martian.proxy/configure", nil)
	if err != nil {
		t.Fatalf("NewRequest(): got %v, want no error", err)
	}

	martian.TestContext(req, nil, nil)

	if err := forwarder.ModifyRequest(req); err != nil {
		t.Fatalf("ModifyRequest(): got %v, want no error", err)
	}

	if got, want := req.URL.Host, "example.com:8181"; got != want {
		t.Errorf("req.URL.Host: got %s, want %s", got, want)
	}
}
