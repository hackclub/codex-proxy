package secretbox

import "testing"

func TestRoundTrip(t *testing.T) {
	box, err := New("this-is-a-test-secret-with-more-than-32-characters")
	if err != nil {
		t.Fatal(err)
	}

	first, err := box.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("encryption reused a nonce")
	}

	plaintext, err := box.Decrypt(first)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != "refresh-token" {
		t.Fatalf("got %q", plaintext)
	}
}

func TestWrongKeyFails(t *testing.T) {
	first, _ := New("first-test-secret-with-more-than-32-characters")
	second, _ := New("second-test-secret-with-more-than-32-characters")
	ciphertext, _ := first.Encrypt("refresh-token")
	if _, err := second.Decrypt(ciphertext); err == nil {
		t.Fatal("decrypt succeeded with the wrong key")
	}
}

func TestRejectsShortKey(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("accepted a short key")
	}
}
