package app

import "testing"

func TestValidSignature(t *testing.T) {
	t.Parallel()
	body := []byte(`{"eventId":"evt-1","status":"paid"}`)
	if !validSignature(body, "4c1c06a47e20c4614d3b34c35c9f79ad10e99c72b98455d303dd17d6b9e691e8", "secret") {
		t.Fatal("valid HMAC signature rejected")
	}
	if validSignature(body, "4c1c06a47e20c4614d3b34c35c9f79ad10e99c72b98455d303dd17d6b9e691e9", "secret") {
		t.Fatal("invalid HMAC signature accepted")
	}
}

func TestPositiveInt(t *testing.T) {
	t.Parallel()
	if value, ok := positiveInt("", 20, 1, 100); !ok || value != 20 {
		t.Fatal("default page size was not applied")
	}
	if _, ok := positiveInt("101", 20, 1, 100); ok {
		t.Fatal("out-of-range page size accepted")
	}
	if _, ok := positiveInt("abc", 20, 1, 100); ok {
		t.Fatal("non-numeric page size accepted")
	}
}
