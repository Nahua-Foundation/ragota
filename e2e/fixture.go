//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// The fixture estate: two tiny Go services joined by one HTTP contract.
// gateway POSTs to /charges, payments serves it, so the indexer has a real
// cross-repository edge (http:POST /charges) to find — the thing this
// project exists to answer questions about.
//
// The wording is deliberate. The payments doc comment carries the terms the
// search phase asks for (capture, withdraw, card), and the gateway avoids
// them, so "where is the charge captured" has exactly one right repository.

const paymentsGoMod = `module payments

go 1.22
`

const paymentsMain = `// Command payments settles card charges for the shop.
package main

import (
	"encoding/json"
	"net/http"
)

// A Charge is one settled payment.
type Charge struct {
	ID     string ` + "`" + `json:"id"` + "`" + `
	Amount int    ` + "`" + `json:"amount_cents"` + "`" + `
}

// CaptureCharge withdraws the authorised amount from the customer's card once
// the payment is approved, and answers with the settled charge.
func CaptureCharge(w http.ResponseWriter, r *http.Request) {
	var c Charge
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.ID = "ch_1"
	_ = json.NewEncoder(w).Encode(c)
}

func main() {
	http.HandleFunc("POST /charges", CaptureCharge)
	_ = http.ListenAndServe(":9090", nil)
}
`

const gatewayGoMod = `module gateway

go 1.22
`

const gatewayMain = `// Command gateway fronts the shop: it accepts a checkout and forwards the
// settlement to the payments service.
package main

import (
	"bytes"
	"net/http"
)

// submitCheckout forwards the order total to the payments service.
func submitCheckout(w http.ResponseWriter, r *http.Request) {
	body := bytes.NewReader([]byte(` + "`" + `{"amount_cents":1200}` + "`" + `))
	req, err := http.NewRequest("POST", "http://payments.internal:9090/charges", body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
}

func main() {
	http.HandleFunc("POST /checkout", submitCheckout)
	_ = http.ListenAndServe(":8081", nil)
}
`

// fixture is the on-disk estate the tests point --source at.
type fixture struct {
	// root holds both repositories; a --source here activates the whole estate.
	root string
	// gateway is the gateway repository itself; a --source here narrows the
	// working set to one repository and leaves payments dormant.
	gateway string
}

// writeFixture lays the two repositories out under a temp dir. A directory is
// a repository to discovery when it contains .git — an empty directory is
// enough, the local source reads files straight from the tree.
func writeFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"payments/go.mod":  paymentsGoMod,
		"payments/main.go": paymentsMain,
		"gateway/go.mod":   gatewayGoMod,
		"gateway/main.go":  gatewayMain,
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	for _, repo := range []string{"payments", "gateway"} {
		if err := os.MkdirAll(filepath.Join(root, repo, ".git"), 0o755); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	return fixture{root: root, gateway: filepath.Join(root, "gateway")}
}
