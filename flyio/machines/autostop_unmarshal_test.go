package machines

import (
	"encoding/json"
	"testing"
)

func TestFlyMachineServiceAutostopUnmarshalJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      string
		want    FlyMachineServiceAutostop
		wantErr bool
	}{
		{name: "string off", in: `"off"`, want: Off},
		{name: "string stop", in: `"stop"`, want: Stop},
		{name: "string suspend", in: `"suspend"`, want: Suspend},
		{name: "bool false maps to off", in: `false`, want: Off},
		{name: "bool true maps to stop", in: `true`, want: Stop},
		{name: "number is rejected", in: `1`, wantErr: true},
		{name: "object is rejected", in: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got FlyMachineServiceAutostop
			err := json.Unmarshal([]byte(tc.in), &got)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Unmarshal(%s): err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("Unmarshal(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
