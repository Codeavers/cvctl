package upgrade

import (
	"reflect"
	"testing"
)

func TestDiff(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		want     []Change
		wantErr  bool
	}{
		{"iguales", `{"a":{"b":1}}`, `{"a":{"b":1}}`, nil, false},
		{
			"cambio de cadena",
			`{"instances":{"cute-prod":{"data":{"media":{"backup":"volume"}}}}}`,
			`{"instances":{"cute-prod":{"data":{"media":{"backup":"none"}}}}}`,
			[]Change{{Path: "instances.cute-prod.data.media.backup", From: "volume", To: "none"}},
			false,
		},
		{"solo en to", `{"a":1}`, `{"a":1,"b":true}`, []Change{{Path: "b", From: "(absent)", To: "true"}}, false},
		{"solo en from", `{"a":1,"b":[1,2]}`, `{"a":1}`, []Change{{Path: "b", From: "[1,2]", To: "(absent)"}}, false},
		{"lista distinta", `{"l":[1,2]}`, `{"l":[1,3]}`, []Change{{Path: "l", From: "[1,2]", To: "[1,3]"}}, false},
		{"tipo distinto", `{"a":{"x":1}}`, `{"a":"s"}`, []Change{{Path: "a", From: `{"x":1}`, To: "s"}}, false},
		{
			"ordenado por path",
			`{"z":1,"a":1,"m":{"k":1}}`,
			`{"z":2,"a":2,"m":{"k":2}}`,
			[]Change{{"a", "1", "2"}, {"m.k", "1", "2"}, {"z", "1", "2"}},
			false,
		},
		{"from inválido", `{`, `{}`, nil, true},
		{"to inválido", `{}`, `x`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Diff([]byte(tt.from), []byte(tt.to))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
