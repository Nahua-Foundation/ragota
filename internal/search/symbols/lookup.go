package symbols

import (
	"context"
	"strings"

	"ragota/internal/store"
)

// FindDefinition ищет определения символа (любой kind, кроме module).
func (s *Service) FindDefinition(ctx context.Context, symbol string) ([]store.ASTUnit, error) {
	units, err := s.st.FindASTUnits(ctx, symbol, "", "", "", 100)
	if err != nil {
		return nil, err
	}
	out := []store.ASTUnit{}
	hasExactNonModule := false
	for _, u := range units {
		if u.Kind == "module" {
			continue
		}
		if strings.EqualFold(u.Name, symbol) || strings.EqualFold(u.Qualified, symbol) {
			hasExactNonModule = true
			break
		}
	}

	for _, u := range units {
		if u.Kind == "module" {
			continue
		}
		if hasExactNonModule {
			if strings.EqualFold(u.Name, symbol) || strings.EqualFold(u.Qualified, symbol) {
				out = append(out, u)
			}
		} else {
			out = append(out, u)
		}
	}
	return out, nil
}

// findCallable ищет function/method по имени.
func (s *Service) findCallable(ctx context.Context, name string) ([]store.ASTUnit, error) {
	all := []store.ASTUnit{}
	for _, k := range []string{"function", "method"} {
		us, err := s.st.FindASTUnits(ctx, name, k, "", "", 100)
		if err != nil {
			return nil, err
		}
		all = append(all, us...)
	}
	return all, nil
}
