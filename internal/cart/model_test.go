package cart

import "testing"

func TestAddOrMergeFoldsRepeatedProducts(t *testing.T) {
	c := NewCart("alice")
	c.AddOrMerge(Item{ProductID: "p1", Quantity: 2, UnitWeight: 1.5, UnitPrice: 10})
	c.AddOrMerge(Item{ProductID: "p1", Quantity: 3, UnitWeight: 1.5, UnitPrice: 10})
	c.AddOrMerge(Item{ProductID: "p2", Quantity: 1, UnitWeight: 0.5, UnitPrice: 4})

	if len(c.Items) != 2 {
		t.Fatalf("cart holds %d lines, want 2", len(c.Items))
	}
	if c.Items[0].Quantity != 5 {
		t.Errorf("p1 quantity = %d, want 5", c.Items[0].Quantity)
	}
	if got, want := c.TotalWeight(), 8.0; got != want {
		t.Errorf("TotalWeight = %v, want %v", got, want)
	}
	if got, want := c.TotalPrice(), 54.0; got != want {
		t.Errorf("TotalPrice = %v, want %v", got, want)
	}
}

// Adding more of a product picks up the price the customer was just shown,
// rather than silently keeping a price from an earlier session.
func TestAddOrMergeRefreshesTheSnapshot(t *testing.T) {
	c := NewCart("alice")
	c.AddOrMerge(Item{ProductID: "p1", Quantity: 1, UnitPrice: 10, UnitWeight: 1})
	c.AddOrMerge(Item{ProductID: "p1", Quantity: 1, UnitPrice: 12, UnitWeight: 1})

	if c.Items[0].UnitPrice != 12 {
		t.Errorf("unit price = %v, want 12", c.Items[0].UnitPrice)
	}
}

func TestTotalsRoundToCents(t *testing.T) {
	c := NewCart("alice")
	c.AddOrMerge(Item{ProductID: "p1", Quantity: 3, UnitPrice: 19.99, UnitWeight: 0.1})

	if got, want := c.TotalPrice(), 59.97; got != want {
		t.Errorf("TotalPrice = %v, want %v", got, want)
	}
	if got, want := c.TotalWeight(), 0.3; got != want {
		t.Errorf("TotalWeight = %v, want %v", got, want)
	}
}

func TestCartIDRoundTrip(t *testing.T) {
	id := IDFor("cust-42")
	if id != "cart-cust-42" {
		t.Fatalf("IDFor = %q", id)
	}

	got, err := CustomerFor(id)
	if err != nil {
		t.Fatalf("CustomerFor: %v", err)
	}
	if got != "cust-42" {
		t.Errorf("CustomerFor = %q, want cust-42", got)
	}
}

func TestCustomerForRejectsMalformedIDs(t *testing.T) {
	for _, in := range []string{"", "cust-42", "cart-", "shopping-cart-1"} {
		if _, err := CustomerFor(in); err == nil {
			t.Errorf("CustomerFor(%q) succeeded, want an error", in)
		}
	}
}

func TestOrderItemsDropPricing(t *testing.T) {
	c := NewCart("alice")
	c.AddOrMerge(Item{ProductID: "p1", Quantity: 2, UnitPrice: 10, UnitWeight: 1})

	got := c.OrderItems()
	if len(got) != 1 || got[0].ProductID != "p1" || got[0].Quantity != 2 {
		t.Fatalf("OrderItems = %+v", got)
	}
}
