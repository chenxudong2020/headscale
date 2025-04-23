package hscontrol

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/juanfont/headscale/hscontrol/types"
)

type WebConfig struct {
	types.Config
	TargetURL string
}

func (h *Headscale) AdminHandler(
	writer http.ResponseWriter,
	req *http.Request,
) {
	// 重定向到后台管理地址
	targetURL := fmt.Sprintf("%s/admin/", strings.TrimSuffix(h.cfg.TargetURL, "/"))
	writer.Header().Set("Location", targetURL)
	writer.WriteHeader(http.StatusFound)
}

func (a *AuthProviderWeb) WebRegisterHandler(
	writer http.ResponseWriter,
	req *http.Request,
) {
	vars := mux.Vars(req)
	registrationIdStr := vars["registration_id"]

	// We need to make sure we dont open for XSS style injections, if the parameter that
	// is passed as a key is not parsable/validated as a NodePublic key, then fail to render
	// the template and log an error.
	registrationId, err := types.RegistrationIDFromString(registrationIdStr)
	if err != nil {
		httpError(writer, NewHTTPError(http.StatusBadRequest, "invalid registration id", err))
		return
	}

	//writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	//writer.WriteHeader(http.StatusOK)
	//writer.Write([]byte(templates.RegisterWeb(registrationId, a.targetURL).Render()))

	// 先拼接成完整的注册地址
	targetURL := fmt.Sprintf("%s/register/%s", strings.TrimSuffix(a.targetURL, "/"), registrationId.String())
	writer.Header().Set("Location", targetURL)
	writer.WriteHeader(http.StatusFound)
}
