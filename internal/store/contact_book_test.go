package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// Fictional identities only.
const (
	bookPN  = "15550000001@s.whatsapp.net"
	bookLID = "100000000001@lid"
)

func TestSetContactBookNameReplacesSavedNamesOnly(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertContact(bookPN, "15550000001", "Sammy", "Old Name", "Old", "Biz"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSystemName(bookPN, "System Sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactBookName(bookPN, "15550000001", "Sam", "Sam Example", bookLID); err != nil {
		t.Fatalf("SetContactBookName: %v", err)
	}
	var first, full, push, business, system sql.NullString
	if err := db.sql.QueryRow(`SELECT first_name, full_name, push_name, business_name, system_name FROM contacts WHERE jid = ?`, bookPN).
		Scan(&first, &full, &push, &business, &system); err != nil {
		t.Fatal(err)
	}
	if first.String != "Sam" || full.String != "Sam Example" || push.String != "Sammy" || business.String != "Biz" || system.String != "System Sam" {
		t.Fatalf("row = first %q full %q push %q business %q system %q", first.String, full.String, push.String, business.String, system.String)
	}
	// The other identity is only updated when its row already exists.
	if _, err := db.GetContact(bookLID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("LID row = %v, want none created", err)
	}

	if err := db.ClearContactBookName(bookPN, bookLID); err != nil {
		t.Fatalf("ClearContactBookName: %v", err)
	}
	if err := db.sql.QueryRow(`SELECT first_name, full_name, push_name FROM contacts WHERE jid = ?`, bookPN).Scan(&first, &full, &push); err != nil {
		t.Fatal(err)
	}
	if first.Valid || full.Valid || push.String != "Sammy" {
		t.Fatalf("after clear: first %v full %v push %q", first, full, push.String)
	}
}

func TestSetContactBookNameCreatesPhoneRow(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertContact(bookLID, "", "", "Old Name", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactBookName(bookPN, "15550000001", "", "Sam Example", bookLID); err != nil {
		t.Fatal(err)
	}
	for _, jid := range []string{bookPN, bookLID} {
		c, err := db.GetContact(jid)
		if err != nil || c.Name != "Sam Example" {
			t.Fatalf("%s = %+v, %v", jid, c, err)
		}
	}
	c, _ := db.GetContact(bookPN)
	if c.Phone != "15550000001" {
		t.Fatalf("phone = %q", c.Phone)
	}
}

func TestContactBlocks(t *testing.T) {
	db := openTestDB(t)
	at := time.Unix(1790000000, 0)
	blocked := func(jids ...string) bool {
		t.Helper()
		got, err := db.AnyContactBlocked(jids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if blocked(bookPN) || blocked() {
		t.Fatal("empty block list reports a block")
	}
	if err := db.SetContactsBlocked([]string{bookPN, bookLID}, true, at); err != nil {
		t.Fatal(err)
	}
	if !blocked(bookLID) || !blocked("12025550142@s.whatsapp.net", bookPN) {
		t.Fatal("blocked identities not found")
	}
	if err := db.ReplaceContactBlocks([]string{"12025550142@s.whatsapp.net"}, at); err != nil {
		t.Fatal(err)
	}
	if blocked(bookPN, bookLID) || !blocked("12025550142@s.whatsapp.net") {
		t.Fatal("replace did not install exactly the fetched list")
	}
	if err := db.SetContactsBlocked([]string{"12025550142@s.whatsapp.net"}, false, at); err != nil {
		t.Fatal(err)
	}
	if blocked("12025550142@s.whatsapp.net") {
		t.Fatal("unblock left the identity blocked")
	}
	if err := db.ReplaceContactBlocks(nil, at); err != nil {
		t.Fatal(err)
	}
}

func TestAnyContactBlockedToleratesStoreWithoutTable(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.sql.Exec(`DROP TABLE contact_blocks`); err != nil {
		t.Fatal(err)
	}
	if got, err := db.AnyContactBlocked([]string{bookPN}); err != nil || got {
		t.Fatalf("AnyContactBlocked = %t, %v; want false without the table", got, err)
	}
}
