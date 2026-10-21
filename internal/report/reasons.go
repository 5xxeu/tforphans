package report

import "github.com/5xxeu/tforphans/internal/model"

func reasonReference(code string) string {
	if link := model.ReasonLink(code); link != "" {
		return " (" + link + ")"
	}
	return ""
}
