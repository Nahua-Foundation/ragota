package httpx

import "testing"

func TestHostPort(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "localhost:7301", want: "localhost:7301"},
		{in: "http://localhost:6333", want: "localhost:6333"},
		{in: "https://api.openai.com", want: "api.openai.com:443"},
		{in: "http://ollama", want: "ollama:80"},
		{in: "http://vllm:8000/v1", want: "vllm:8000"},
		{in: "localhost", wantErr: true},
		{in: "ftp://host/x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := HostPort(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("HostPort(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("HostPort(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("HostPort(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
