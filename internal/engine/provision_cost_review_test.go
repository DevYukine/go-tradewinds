package engine

import (
	"encoding/json"
	"math"
	"testing"
)

func TestProvisionConsumptionIncludesEachFillCustomsRounding(t *testing.T) {
	t.Parallel()
	var book Market
	if err := json.Unmarshal([]byte(`{
		"port_id":"source","visibility":"quay","customs_bps":1,
		"books":[{"commodity_id":"provisions","depth":{"asks":[
			{"order_id":"first","price_pence":1,"remaining_quantity":1},
			{"order_id":"second","price_pence":1,"remaining_quantity":1}
		]}}]
	}`), &book); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		consumed float64
		want     int64
	}{
		{consumed: 2, want: 4},
		{consumed: 1.5, want: 3},
		{consumed: 1, want: 2},
	} {
		got, err := provisionsExpense(book, test.consumed)
		if err != nil || got != test.want {
			t.Fatalf("consumed %g tons: got %dp, want %dp, error %v", test.consumed, got, test.want, err)
		}
	}
	if _, err := provisionsExpense(book, 3); err == nil {
		t.Fatal("missing replacement depth was valued as available provisions")
	}
	book.Books[0].Depth.Asks[0].RemainingQuantity = 2
	book.Books[0].Depth.Asks = book.Books[0].Depth.Asks[:1]
	if got, err := provisionsExpense(book, 2); err != nil || got != 3 {
		t.Fatalf("one two-ton fill paid two independently rounded fees: %dp, %v", got, err)
	}
}

func TestProvisionReplacementRejectsUnrepresentableCash(t *testing.T) {
	t.Parallel()
	var book Market
	if err := json.Unmarshal([]byte(`{
		"books":[{"commodity_id":"provisions","depth":{"asks":[
			{"order_id":"only","price_pence":10000,"remaining_quantity":1}
		]}}]
	}`), &book); err != nil {
		t.Fatal(err)
	}
	book.CustomsBPS = math.MaxInt64
	if _, err := provisionsExpense(book, 1); err == nil {
		t.Fatal("replacement cash overflow was admitted as an operating cost")
	}
}
