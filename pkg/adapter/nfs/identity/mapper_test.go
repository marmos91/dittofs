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
