// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"encoding/json"
	"sync"

	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"

	"golang.org/x/net/http2"

	"google.golang.org/api/idtoken"
	"google.golang.org/api/impersonate"

	"golang.org/x/oauth2"

	"github.com/gorilla/mux"
	"golang.org/x/oauth2/google"
)

var (
	cfg         = &serverConfig{}
	hostHeaders = []string{"metadata", "metadata.google.internal", "169.254.169.254"}

	customAttributeMap = map[string]string{"k1": "v1", "k2": "v2"}

	tokenMutex = &sync.Mutex{}

	creds *google.Credentials
)

const (
	emailScope = "https://www.googleapis.com/auth/userinfo.email"

	googleProjectID        = "GOOGLE_PROJECT_ID"
	googleNumericProjectID = "GOOGLE_NUMERIC_PROJECT_ID"
	googleAccessToken      = "GOOGLE_ACCESS_TOKEN"
	googleIDToken          = "GOOGLE_ID_TOKEN"
	googleAccountEmail     = "GOOGLE_ACCOUNT_EMAIL"
)

type serverConfig struct {
	flPort                string
	flnumericProjectID    string
	fltokenScopes         string
	flprojectID           string
	flserviceAccountEmail string
	flserviAccountFile    string
    flcustomAttributeFile string
	flImpersonate         bool
}

type metadataToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

type serviceAccountDetails struct {
	Aliases string `json:"aliases"`
	Email   string `json:"email"`
	Scopes  string `json:"scopes"`
}

func getAccessToken() (*metadataToken, error) {
	tokenMutex.Lock()
	defer tokenMutex.Unlock()

	if isEnvironmentOverrideSet() {
		// access_token is opaque but you _can_ get the exp
		// time by calling  curl https://www.googleapis.com/oauth2/v3/tokeninfo?access_token=
		// ...but i don't see it necessary to populate the expiration field, besides
		// https://godoc.org/golang.org/x/oauth2#Token
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{
				AccessToken: os.Getenv(googleAccessToken),
				//Expiry:      time.Now().Add(time.Hour * 1),
				TokenType: "Bearer",
			},
		)
		creds = &google.Credentials{
			ProjectID:   os.Getenv(googleProjectID),
			TokenSource: ts,
		}
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		glog.Error(err)
		return &metadataToken{}, err
	}

	loc, _ := time.LoadLocation("UTC")
	now := time.Now().In(loc)
	diff := tok.Expiry.Sub(now)
	return &metadataToken{
		AccessToken: tok.AccessToken,
		ExpiresIn:   int(diff.Round(time.Second).Seconds()),
		TokenType:   tok.TokenType,
	}, nil

}

func getIDToken(targetAudience string) (string, error) {
	tokenMutex.Lock()
	defer tokenMutex.Unlock()
	if isEnvironmentOverrideSet() {
		return os.Getenv(googleIDToken), nil
	}
	var idTokenSource oauth2.TokenSource
	var err error

	ctx := context.Background()
	if cfg.flImpersonate {

		idTokenSource, err = impersonate.IDTokenSource(ctx,
			impersonate.IDTokenConfig{
				TargetPrincipal: cfg.flserviceAccountEmail,
				Audience:        targetAudience,
				IncludeEmail:    true,
			},
		)
	} else {
		idTokenSource, err = idtoken.NewTokenSource(ctx, targetAudience, idtoken.WithCredentialsJSON(creds.JSON))
	}
	if err != nil {
		glog.Errorln(err)
		return "", errors.New("unable to get id_token")
	}
	tok, err := idTokenSource.Token()
	if err != nil {
		glog.Error(err)
		return "", err
	}
	return tok.AccessToken, nil
}

func getProjectID() string {
	if isEnvironmentOverrideSet() {
		return os.Getenv(googleProjectID)
	} else if cfg.flprojectID != "" {
		return cfg.flprojectID
	}
	return creds.ProjectID
}

func getNumericProjectID() string {
	if isEnvironmentOverrideSet() {
		return os.Getenv(googleNumericProjectID)
	}
	return cfg.flnumericProjectID
}

func getServiceAccountEmail() string {
	if isEnvironmentOverrideSet() {
		return os.Getenv(googleAccountEmail)
	}
	if cfg.flserviceAccountEmail != "" {
		return cfg.flserviceAccountEmail
	}
	conf, err := google.JWTConfigFromJSON(creds.JSON, emailScope)
	if err != nil {
		glog.Errorf("unable to get serviceAccountEmail from JSON certificate file %v", err)
		os.Exit(1)
	}
	return conf.Email
}

func checkMetadataHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		glog.V(10).Infof("Got Request: %v", r)
		w.Header().Add("Server", "Metadata Server for VM")
		w.Header().Add("Metadata-Flavor", "Google")
		w.Header().Add("X-XSS-Protection", "0")
		w.Header().Add("X-Frame-Options", "0")

		hasHostHeader := false
		for _, a := range hostHeaders {
			if a == r.Host {
				hasHostHeader = true
			}
		}

		if !hasHostHeader {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			return
		}
		flavor := r.Header.Get("Metadata-Flavor")
		if flavor == "" && r.RequestURI != "/" {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	glog.Infoln("/ called")

	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	fmt.Fprint(w, "ok")
}

func projectIDHandler(w http.ResponseWriter, r *http.Request) {
	glog.Infoln("/computeMetadata/v1/project/project-id called")
	fmt.Fprint(w, getProjectID())
}

func numericProjectIDHandler(w http.ResponseWriter, r *http.Request) {
	glog.Infoln("/computeMetadata/v1/project/numeric-project-id called")
	fmt.Fprint(w, getNumericProjectID())
}

func attributesHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	glog.Infof("/computeMetadata/v1/project/attributes/{k} called for attribute %v", vars["key"])

	if val, ok := customAttributeMap[vars["key"]]; ok {
		fmt.Fprint(w, val)
	} else {
		fmt.Fprint(w, http.StatusNotFound)
	}
}

func listServiceAccountHandler(w http.ResponseWriter, r *http.Request) {
	glog.Infoln("/computeMetadata/v1/instance/service-accounts/ called")
	// TODO: its possible the vm doens't have a svc-account
	w.Header().Add("Content-Type", "application/text")
	fmt.Fprint(w, "default/\n"+getServiceAccountEmail()+"/\n")
}

func getServiceAccountIndexHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	glog.Infof("/computeMetadata/v1/instance/service-accounts/%v/ called", vars["acct"])
	// TODO: its possible the vm doens't have a svc-account

	var scopes string
	for _, e := range strings.Split(cfg.fltokenScopes, ",") {
		scopes = scopes + e + "\n"
	}

	js, err := json.Marshal(&serviceAccountDetails{
		Aliases: vars["acct"],
		Email:   getServiceAccountEmail(),
		Scopes:  scopes,
	})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		w.Header().Set("Content-Type", "applicaiton/text")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(js)

}

func notFound(w http.ResponseWriter, r *http.Request) {
	glog.Infof("%s called but is not implemented", r.URL.Path)
	http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
}

func getServiceAccountHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	glog.Infof("/computeMetadata/v1/instance/service-accounts/%v/%v called", vars["acct"], vars["key"])

	switch vars["key"] {

	case "aliases":
		w.Header().Set("Content-Type", "application/text")
		fmt.Fprint(w, "default")

	case "email":
		w.Header().Set("Content-Type", "application/text")
		fmt.Fprint(w, getServiceAccountEmail())

	case "identity":
		k, ok := r.URL.Query()["audience"]
		if !ok {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "non-empty audience parameter required")
			return
		}
		idtok, err := getIDToken(k[0])
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			w.Header().Set("Content-Type", "text/html")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, idtok)

	case "scopes":

		var scopes string
		for _, e := range strings.Split(cfg.fltokenScopes, ",") {
			scopes = scopes + e + "\n"
		}
		w.Header().Set("Content-Type", "application/text")
		fmt.Fprint(w, scopes)

	case "token":
		tok, err := getAccessToken()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			w.Header().Set("Content-Type", "applicaiton/text")
			return
		}
		js, err := json.Marshal(tok)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			w.Header().Set("Content-Type", "applicaiton/text")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(js)

	default:
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		return
	}

}

func isEnvironmentOverrideSet() bool {
	if os.Getenv(googleAccessToken) != "" && os.Getenv(googleIDToken) != "" && os.Getenv(googleAccountEmail) != "" && os.Getenv(googleNumericProjectID) != "" && os.Getenv(googleProjectID) != "" {
		return true
	}
	return false
}

func setCustomAttributes(customAttributesFile string) {
    if customAttributesFile == "" {
        return
    }
    file, err := os.Open(customAttributesFile)
    if err != nil {
        //log.Fatal(err)
        glog.Error("Can't Open Custom Attributes file " + customAttributesFile)
        return
    }
    defer file.Close()
    var data map[string]string
    if err := json.NewDecoder(file).Decode(&data); err != nil {
        //glog.Fatal(err)
        glog.Error("Can't parse file " + customAttributesFile + " (expected json file)")
        return
    }
    
    fmt.Printf("%#v", data)
    customAttributeMap = data
}

func main() {
	ctx := context.Background()
	flag.StringVar(&cfg.flPort, "port", ":8080", "port...")
	flag.StringVar(&cfg.flnumericProjectID, "numericProjectId", "", "numericProjectId...")
	flag.StringVar(&cfg.fltokenScopes, "tokenScopes", "https://www.googleapis.com/auth/userinfo.email", "tokenScopes")
	flag.StringVar(&cfg.flprojectID, "projectId", "", "projectId...")
	flag.StringVar(&cfg.flserviceAccountEmail, "serviceAccountEmail", "", "serviceAccountEmail...")
	flag.StringVar(&cfg.flserviAccountFile, "serviceAccountFile", "", "serviceAccountFile...")
	flag.StringVar(&cfg.flcustomAttributeFile, "customAttributeFile", "", "customAttributeFile - json of custom attributes ({ key:val}) - OPTIONAL ")
	flag.BoolVar(&cfg.flImpersonate, "impersonate", false, "Impersonate a service Account instead of using the keyfile")
	flag.Parse()

	argError := func(s string, v ...interface{}) {
		flag.PrintDefaults()
		glog.Errorf("Invalid Argument error: "+s, v...)
		os.Exit(-1)
	}


    

	glog.Infof("Starting GCP metadataserver on port, %v", cfg.flPort)

	r := mux.NewRouter()
	r.StrictSlash(true)
	r.Handle("/computeMetadata/v1/project/project-id", checkMetadataHeaders(http.HandlerFunc(projectIDHandler))).Methods("GET")
	r.Handle("/computeMetadata/v1/project/numeric-project-id", checkMetadataHeaders(http.HandlerFunc(numericProjectIDHandler))).Methods("GET")
	r.Handle("/computeMetadata/v1/project/attributes/{key}", checkMetadataHeaders(http.HandlerFunc(attributesHandler))).Methods("GET")
	r.Handle("/computeMetadata/v1/instance/service-accounts/", checkMetadataHeaders(http.HandlerFunc(listServiceAccountHandler))).Methods("GET")
	r.Handle("/computeMetadata/v1/instance/service-accounts/{acct}/", checkMetadataHeaders(http.HandlerFunc(getServiceAccountIndexHandler))).Methods("GET")
	r.Handle("/computeMetadata/v1/instance/service-accounts/{acct}/{key}", checkMetadataHeaders(http.HandlerFunc(getServiceAccountHandler))).Methods("GET")
	r.Handle("/", checkMetadataHeaders(http.HandlerFunc(rootHandler))).Methods("GET")
	r.NotFoundHandler = checkMetadataHeaders(http.HandlerFunc(notFound))
	//r.Handle("/", checkMetadataHeaders(http.FileServer(http.Dir("./static"))))
	http.Handle("/", r)

	srv := &http.Server{
		Addr: cfg.flPort,
	}
	http2.ConfigureServer(srv, &http2.Server{})

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// First check if env-var based overrides are set.  We need all of them to be set for the
	// client libraries.  We are _not_ going to set a credential object here but read it on request.
	// TODO: make the credential and runtime source data an adapter: eg, token, projectiD, etc
	//       gets read in from a variety of sources (args+svcAccountFile, env vars, kubernetes secrets)
	// serviceAccountFile based credentials isn't necessary if env-var based settings are used.
	// technically, you could mix and match env var and svc-account values but that makes it
	// pretty confusing...so I'll just go w/ one or the other

	if isEnvironmentOverrideSet() {
		glog.Infoln("Using environment variables for credentials")
	} else if cfg.flImpersonate {
		glog.Infoln("Using Service Account Impersonation")

		if cfg.flnumericProjectID == "" || cfg.flprojectID == "" || cfg.flserviceAccountEmail == "" {
			argError("projectId,numericProjectId,serviceAccountEmail must be set if impersonation is used")
		}

		var err error
		s := strings.Split(cfg.fltokenScopes, ",")
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: cfg.flserviceAccountEmail,
			Scopes:          s,
		})
		if err != nil {
			glog.Errorf("Unable to create Impersonated TokenSource %v ", err)
			os.Exit(1)
		}

		creds = &google.Credentials{
			ProjectID:   cfg.flprojectID,
			TokenSource: ts,
		}

	} else {

		if cfg.flserviAccountFile == "" {
			argError("Either environment variable overides or -serviceAccountFile must be specified")
		}

		glog.Infoln("Using serviceAccountFile for credentials")
		var err error
		//creds, err = google.FindDefaultCredentials(ctx, tokenScopes)
		data, err := ioutil.ReadFile(cfg.flserviAccountFile)
		if err != nil {
			glog.Errorf("Unable to read serviceAccountFile %v", err)
			os.Exit(1)
		}
		s := strings.Split(cfg.fltokenScopes, ",")
		creds, err = google.CredentialsFromJSON(ctx, data, s...)
		if err != nil {
			glog.Errorf("Unable to parse serviceAccountFile %v ", err)
			os.Exit(1)
		}
	}

    setCustomAttributes(cfg.flcustomAttributeFile)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			glog.Fatalf("listen: %s\n", err)
		}
	}()
	glog.Infoln("Server Started")
	<-done
	glog.Infoln("Server Stopped")

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server Shutdown Failed:%+v", err)
	}
	glog.Infoln("Server Exited Properly")

}
