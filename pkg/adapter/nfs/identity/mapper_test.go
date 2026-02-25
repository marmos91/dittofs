package identity

import "testing"

// ============================================================================
// ParsePrincipal tests
// ============================================================================

func TestParsePrincipal_UserAtDomain(t *testing.T) {
	name, domain := ParsePrincipal("alice@EXAMPLE.COM")
	if name != "alice" {
		t.Fatalf("expected name=alice, got %s", name)
	}
	if domain != "EXAMPLE.COM" {
		t.Fatalf("expected domain=EXAMPLE.COM, got %s", domain)
	}
}

func TestParsePrincipal_NumericAtDomain(t *testing.T) {
	name, domain := ParsePrincipal("1000@localdomain")
	if name != "1000" {
		t.Fatalf("expected name=1000, got %s", name)
	}
	if domain != "localdomain" {
		t.Fatalf("expected domain=localdomain, got %s", domain)
	}
}

func TestParsePrincipal_NoDomain(t *testing.T) {
	name, domain := ParsePrincipal("alice")
	if name != "alice" {
		t.Fatalf("expected name=alice, got %s", name)
	}
	if domain != "" {
		t.Fatalf("expected empty domain, got %s", domain)
	}
}

func TestParsePrincipal_SpecialOwner(t *testing.T) {
	name, domain := ParsePrincipal("OWNER@")
	if name != "OWNER@" {
		t.Fatalf("expected name=OWNER@, got %s", name)
	}
	if domain != "" {
		t.Fatalf("expected empty domain, got %s", domain)
	}
}

func TestParsePrincipal_SpecialGroup(t *testing.T) {
	name, domain := ParsePrincipal("GROUP@")
	if name != "GROUP@" {
		t.Fatalf("expected name=GROUP@, got %s", name)
	}
	if domain != "" {
		t.Fatalf("expected empty domain, got %s", domain)
	}
}

func TestParsePrincipal_SpecialEveryone(t *testing.T) {
	name, domain := ParsePrincipal("EVERYONE@")
	if name != "EVERYONE@" {
		t.Fatalf("expected name=EVERYONE@, got %s", name)
	}
	if domain != "" {
		t.Fatalf("expected empty domain, got %s", domain)
	}
}

func TestParsePrincipal_EmptyString(t *testing.T) {
	name, domain := ParsePrincipal("")
	if name != "" {
		t.Fatalf("expected empty name, got %s", name)
	}
	if domain != "" {
		t.Fatalf("expected empty domain, got %s", domain)
	}
}

func TestParsePrincipal_MultipleAt(t *testing.T) {
	// "user@host@REALM" should split on the last @
	name, domain := ParsePrincipal("user@host@REALM")
	if name != "user@host" {
		t.Fatalf("expected name=user@host, got %s", name)
	}
	if domain != "REALM" {
		t.Fatalf("expected domain=REALM, got %s", domain)
	}
}

// ============================================================================
// NobodyIdentity tests
// ============================================================================

func TestNobodyIdentity_Values(t *testing.T) {
	id := NobodyIdentity()
	if id.Username != "nobody" {
		t.Fatalf("expected Username=nobody, got %s", id.Username)
	}
	if id.UID != 65534 {
		t.Fatalf("expected UID=65534, got %d", id.UID)
	}
	if id.GID != 65534 {
		t.Fatalf("expected GID=65534, got %d", id.GID)
	}
	if !id.Found {
		t.Fatal("expected Found=true")
	}
	if len(id.GIDs) != 0 {
		t.Fatalf("expected empty GIDs, got %v", id.GIDs)
	}
	if id.Domain != "" {
		t.Fatalf("expected empty Domain, got %s", id.Domain)
	}
}
