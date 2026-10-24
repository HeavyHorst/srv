package vmrunner

import "testing"

func FuzzStartResultValidate(f *testing.F) {
	f.Add("demo", "instance-1", "request-1", "boot-1")
	f.Add("bad/name", "", "", "")
	f.Fuzz(func(t *testing.T, name, instanceID, requestID, hostBootID string) {
		request := StartResultRequest{Name: name, InstanceID: instanceID, RequestID: requestID, HostBootID: hostBootID}
		err := request.Validate()
		wantErr := !validStartName(name) || instanceID == "" || requestID == "" || hostBootID == ""
		if (err != nil) != wantErr {
			t.Fatalf("Validate(%+v) error = %v, want error %v", request, err, wantErr)
		}
	})
}

// validStartName mirrors the documented name contract without reusing the
// production regexp, so widening that regexp changes only one side of the test.
func validStartName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if c != '-' || i == 0 || i == len(name)-1 {
			return false
		}
	}
	return true
}

func FuzzValidateStartResponse(f *testing.F) {
	f.Add(StartRunning, 1)
	f.Add(StartAbsent, 0)
	f.Add(StartUnknown, 0)
	f.Add(StartRunning, 0)
	f.Add("other", 0)
	f.Fuzz(func(t *testing.T, outcome string, pid int) {
		response := StartResponse{Outcome: outcome, PID: pid}
		err := validateStartResponse(response)
		wantErr := !((outcome == StartRunning && pid > 0) || ((outcome == StartAbsent || outcome == StartUnknown) && pid == 0))
		if (err != nil) != wantErr {
			t.Fatalf("validateStartResponse(%+v) error = %v, want error %v", response, err, wantErr)
		}
	})
}
