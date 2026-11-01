package app

import "testing"

func TestBusCallFollowsTheCallOnTheBus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		frames   []string
		wantType string
		want     string
	}{
		{name: "nothing seen", frames: nil},
		{name: "ringing", frames: []string{"*8*1#1#4#21*11##"}, wantType: "1", want: "11"},
		{name: "declined on the indoor unit", frames: []string{"*8*1#1#4#21*11##", "*8*3#1#4*411##"}},
		{
			// The view frames the companion's own view produced on 2026-11-01,
			// transient call end included.
			name: "view after its transient end",
			frames: []string{
				"*8*1#5#4#20*11##", "*8*9#5#4*20##", "*8*2#5#4*11##", "*8*3#5#4*411##",
				"*8*2#5#4*11##", "*8*100#5#4*20##",
			},
			wantType: "5",
			want:     "11",
		},
		{name: "view ended by the intercom", frames: []string{"*8*1#5#4#20*11##", "*8*2#5#4*11##", "*8*3#5#4*411##"}},
		{name: "answer without a start", frames: []string{"*8*2#1#4*11##"}},
		{name: "unlock frames leave it open", frames: []string{"*8*1#1#4#21*11##", "*8*19*20##", "*8*20*20##"}, wantType: "1", want: "11"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var call busCall
			for _, frame := range test.frames {
				call.observeFrame(frame)
			}

			callType, called, ok := call.take()
			if callType != test.wantType || called != test.want || ok != (test.want != "") {
				t.Fatalf("take() = %q, %q, %t, want %q, %q", callType, called, ok, test.wantType, test.want)
			}

			if _, _, ok := call.take(); ok {
				t.Fatal("second take() reported the call again")
			}
		})
	}
}
