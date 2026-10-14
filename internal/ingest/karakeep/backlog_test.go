package karakeep

import "testing"

// A bookmark that is already an item (for example one brought in from the old
// pile) stays on its own day: the ingestor never moves it to Today.
func TestBookmarkAlreadyInBacklogStaysThere(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name, summary_source) VALUES ('kk1','u1','karakeep','karakeep','Karakeep','karakeep')`)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, karakeep_id, batch_date, created_at, updated_at)
		VALUES ('i1','u1','kk1','b1','article','https://example.com/one','t','light','b1','2025-01-15','c','c')`)
	f.api.marks = append(f.api.marks, mark("b1", "https://example.com/one", 1))
	if res := f.run("u1"); res.Items != 0 {
		t.Fatalf("result = %+v", res)
	}
	var day string
	if err := f.st.DB().QueryRow(`SELECT batch_date FROM items WHERE id = 'i1'`).Scan(&day); err != nil || day != "2025-01-15" {
		t.Fatalf("batch_date = %q, %v", day, err)
	}
}
