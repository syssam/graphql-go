package veloxfx

import (
	"context"
	"testing"

	"github.com/syssam/velox/dialect/sql/sqlgraph"
)

// Stock.quantity is NonNegative(). The ways this service changes a count are
// guarded -- Take and adjustStock both update WHERE quantity >= n -- but the
// validator itself never sees an addition, so the next code path that forgets
// the WHERE would have taken the count below zero in silence. The database
// refuses it (gen.FeatureCheckBounds), on SQLite and on PostgreSQL, and the
// client is told FAILED_PRECONDITION, not that something already exists.
func TestScenarioStockCannotGoNegativeWhateverThePath(t *testing.T) {
	ctx := context.Background()
	client, err := openDB(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	cat := client.Category.Create().SetName("c").SaveX(ctx)
	p := client.Product.Create().SetSku("s").SetName("n").SetPriceCents(100).SetCategoryID(cat.ID).SaveX(ctx)
	w := client.Warehouse.Create().SetName("w").SaveX(ctx)
	st := client.Stock.Create().SetWarehouseID(w.ID).SetProductID(p.ID).SetQuantity(2).SaveX(ctx)

	_, err = client.Stock.UpdateOneID(st.ID).AddQuantity(-3).Save(ctx)
	if !sqlgraph.IsCheckConstraintError(err) {
		t.Fatalf("an unguarded AddQuantity(-3) on 2 in stock: %v", err)
	}
	if got := client.Stock.GetX(ctx, st.ID).Quantity; got != 2 {
		t.Errorf("the refused update changed the count to %d", got)
	}
	if code := presentError(ctx, err).Extensions["code"]; code != "FAILED_PRECONDITION" {
		t.Errorf("reported as %v", code)
	}
	// The bound, not the change, is what is refused.
	if _, err := client.Stock.UpdateOneID(st.ID).AddQuantity(-2).Save(ctx); err != nil {
		t.Errorf("taking the last two: %v", err)
	}
}
