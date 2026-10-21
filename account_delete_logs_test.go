package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// The account-delete handler logs these errors; none may carry the user id
// (it is in the Admin API path and the Storage prefix).
func TestSupabaseAdminErrorsCarryNoUserID(t *testing.T) {
	const userID = "30bd9690-aaaa-bbbb-cccc-ddddeeeeffff"
	srv := httptest.NewServer(nil)
	base := srv.URL
	srv.Close() // connection refused: net/http's *url.Error, which prints the URL

	err := deleteAuthUser(context.Background(), base, "service-role", userID)
	if err == nil {
		t.Fatal("deleteAuthUser against a dead server: no error")
	}
	if strings.Contains(err.Error(), userID) || strings.Contains(err.Error(), "/admin/users") {
		t.Errorf("deleteAuthUser error leaks the user id / URL: %v", err)
	}
	if !strings.Contains(err.Error(), "DELETE") {
		t.Errorf("deleteAuthUser error %q, want the method kept", err)
	}

	err = deleteUserStorageObjects(context.Background(), base, "service-role", userID)
	if err == nil {
		t.Fatal("storage cleanup against a dead server: no error")
	}
	if strings.Contains(err.Error(), userID) {
		t.Errorf("storage cleanup error leaks the user id: %v", err)
	}
}
