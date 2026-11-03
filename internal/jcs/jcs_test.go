package jcs

import "testing"

func TestCanonicalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"b":2,"a":1}`, `{"a":1,"b":2}`},
		{`{ "z" : [ 3 , {"y":true,"x":null} ] }`, `{"z":[3,{"x":null,"y":true}]}`},
		{`"tab\there é"`, `"tab\there é"`},
		{`"\u0001"`, `"\u0001"`},
		{`{"€":1,"\r":2,"1":3}`, `{"\r":2,"1":3,"€":1}`},
		{`-0`, `0`},
		{`9007199254740992`, `9007199254740992`},
	}
	for _, c := range cases {
		got, err := Canonicalize([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: got %s want %s", c.in, got, c.want)
		}
	}
}

func TestRejects(t *testing.T) {
	for _, in := range []string{`1.5`, `1e3`, `9007199254740993`, `{"a":1} {}`} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s: expected error", in)
		}
	}
}

func TestKeyOrderUTF16(t *testing.T) {
	// U+1F600 (surrogate pair D83D) sorts before U+FB33 in UTF-16 but after in code points.
	got, err := Canonicalize([]byte(`{"דּ":1,"😀":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"😀":2,"דּ":1}` {
		t.Fatalf("got %s", got)
	}
}
