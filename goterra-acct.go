package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	yaml "gopkg.in/yaml.v2"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	terraConfig "github.com/osallou/goterra-lib/lib/config"
	terraUser "github.com/osallou/goterra-lib/lib/user"
	"github.com/rs/cors"
	"go.mongodb.org/mongo-driver/bson"
	mongo "go.mongodb.org/mongo-driver/mongo"
	mongoOptions "go.mongodb.org/mongo-driver/mongo/options"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	terraModel "github.com/osallou/goterra-lib/lib/model"
	terraToken "github.com/osallou/goterra-lib/lib/token"

	"github.com/influxdata/influxdb/client/v2"
)

// Version of server
var Version string

var mongoClient mongo.Client
var nsCollection *mongo.Collection
var runCollection *mongo.Collection
var runStateCollection *mongo.Collection
var acctCollection *mongo.Collection
var resourcesCollection *mongo.Collection

//var InfluxHost = "http://localhost:8086" // TODO get from config

//var managedOpenstackResources = []string{"openstack_compute_instance_v2"} // TODO get from config

func isManaged(resource string) bool {
	for _, res := range configAcct.Resources {
		if res == resource {
			return true
		}
	}
	return false
}

// InfluxDB specifies influxdb config
type InfluxDB struct {
	URL      string
	User     string
	Password string
}

// AcctConfig matches acct.yml config
type AcctConfig struct {
	InfluxDB  InfluxDB
	Resources []string
}

// AcctCheck is a unique mongodb record to know when last check was done
type AcctCheck struct {
	ID        int64 `bson:"id"`
	LastCheck int64 `bson:"last_check"`
}

var configAcct AcctConfig

// HomeHandler manages base entrypoint
var HomeHandler = func(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"version": Version, "message": "ok"}
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ResourceHandler returns known used resources (on running deployments)
var ResourceHandler = func(w http.ResponseWriter, r *http.Request) {
	resources := getUsedResources("")
	w.Header().Add("Content-Type", "application/json")
	resp := map[string]interface{}{"resources": resources}
	json.NewEncoder(w).Encode(resp)
	return
}

// ResourceNSHandler returns known used resources (on running deployments) for namespace
var ResourceNSHandler = func(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nsID := vars["id"]
	resources := getUsedResources(nsID)
	w.Header().Add("Content-Type", "application/json")
	resp := map[string]interface{}{"resources": resources}
	json.NewEncoder(w).Encode(resp)
	return
}

// AcctGetHandler returns global stat usage for namespace
var AcctGetHandler = func(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nsID := vars["id"]

	c, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     configAcct.InfluxDB.URL,
		Username: configAcct.InfluxDB.User,
		Password: configAcct.InfluxDB.Password,
	})
	defer c.Close()
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		respError := map[string]interface{}{"message": err.Error}
		json.NewEncoder(w).Encode(respError)
		return
	}

	q := client.Query{
		Command:  fmt.Sprintf("select sum(\"quantity\") as \"quantity\", sum(\"duration\") as \"duration\" from \"goterra.acct\" where \"ns\" = '%s' group by \"resource\",\"kind\";", nsID),
		Database: "goterra",
	}

	if resp, respErr := c.Query(q); respErr == nil {
		w.Header().Add("Content-Type", "application/json")
		acctResults := map[string]interface{}{"acct": resp.Results}
		json.NewEncoder(w).Encode(acctResults)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	respError := map[string]interface{}{"message": "failed to get stats"}
	json.NewEncoder(w).Encode(respError)
	return

}

// AcctGetFromToHandler returns global stat usage for namespace starting between timestamps (seconds), optional query parameter days=1 to get results grouped by days
var AcctGetFromToHandler = func(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nsID := vars["id"]
	from := vars["from"] + "s"
	to := vars["to"] + "s"

	groupByDays := false
	days, ok := r.URL.Query()["days"]
	if ok && days[0] == "1" {
		groupByDays = true
	}

	c, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     configAcct.InfluxDB.URL,
		Username: configAcct.InfluxDB.User,
		Password: configAcct.InfluxDB.Password,
	})
	defer c.Close()
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		respError := map[string]interface{}{"message": err.Error}
		json.NewEncoder(w).Encode(respError)
		return
	}

	timeFilter := ""
	if groupByDays {
		timeFilter = "time(1d),"
	}

	q := client.Query{
		Command:  fmt.Sprintf("select sum(\"quantity\") as \"quantity\", sum(\"duration\") as \"duration\", max(\"quantity\") as \"max\" from \"goterra.acct\" where \"ns\" = '%s' and time > %s and time < %s group by %s\"resource\",\"kind\";", nsID, from, to, timeFilter),
		Database: "goterra",
	}

	if resp, respErr := c.Query(q); respErr == nil {
		w.Header().Add("Content-Type", "application/json")
		acctResults := map[string]interface{}{"acct": resp.Results}
		json.NewEncoder(w).Encode(acctResults)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	respError := map[string]interface{}{"message": "failed to get stats"}
	json.NewEncoder(w).Encode(respError)
	return

}

// AcctGetFromHandler returns global stat usage for namespace starting at from timestamp (seconds)
var AcctGetFromHandler = func(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nsID := vars["id"]
	from := vars["from"] + "s"
	groupByDays := false
	days, ok := r.URL.Query()["days"]
	if ok && days[0] == "1" {
		groupByDays = true
	}
	timeFilter := ""
	if groupByDays {
		timeFilter = "time(1d),"
	}

	c, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     configAcct.InfluxDB.URL,
		Username: configAcct.InfluxDB.User,
		Password: configAcct.InfluxDB.Password,
	})
	defer c.Close()
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		respError := map[string]interface{}{"message": err.Error}
		json.NewEncoder(w).Encode(respError)
		return
	}

	q := client.Query{
		Command:  fmt.Sprintf("select sum(\"quantity\") as \"quantity\", sum(\"duration\") as \"duration\", max(\"quantity\") as \"max\" from \"goterra.acct\" where \"ns\" = '%s' and time > %s group by %s\"resource\",\"kind\";", nsID, from, timeFilter),
		Database: "goterra",
	}

	if resp, respErr := c.Query(q); respErr == nil {
		w.Header().Add("Content-Type", "application/json")
		acctResults := map[string]interface{}{"acct": resp.Results}
		json.NewEncoder(w).Encode(acctResults)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	respError := map[string]interface{}{"message": "failed to get stats"}
	json.NewEncoder(w).Encode(respError)
	return

}

// CheckToken checks Fernet token
func CheckToken(authToken string) (user terraUser.User, err error) {
	// config := terraConfig.LoadConfig()

	tokenStr := strings.Replace(authToken, "Bearer", "", -1)
	tokenStr = strings.TrimSpace(tokenStr)

	msg, errMsg := terraToken.FernetDecode([]byte(tokenStr))
	if errMsg != nil {
		return user, errMsg
	}
	json.Unmarshal(msg, &user)
	return user, nil
}

func getLastVM(now int64, lastCheck int64) []terraModel.Run {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runs := make([]terraModel.Run, 0)
	log.Info().Msgf("Check for VMs not ended or ended after %s", time.Unix(lastCheck, 0))
	filter := bson.M{}
	if lastCheck > 0 {
		filter = bson.M{
			"$or": bson.A{
				bson.M{"end": bson.M{
					"$gt": lastCheck,
				}},
				bson.M{"end": 0},
			},
		}
	}
	cursor, err := runCollection.Find(ctx, filter)
	lastCheck = now
	if err != nil {
		return runs
	}
	for cursor.Next(ctx) {
		var run terraModel.Run
		cursor.Decode(&run)
		log.Debug().Msgf("run = %+v", run)

		runs = append(runs, run)
	}
	//log.Info().Msgf("runs = %+v", runs)
	return runs
}

// StateResources is a structure of terraform show command
type StateResources struct {
	Resources []map[string]interface{} `json:"resources"`
}

// StateValues is a structure of terraform show command
type StateValues struct {
	Outputs    map[string]interface{} `json:"outputs"`
	RootModule StateResources         `json:"root_module"`
}

// State is a structure of terraform show command
type State struct {
	FormatVersion    string      `json:"format_version"`
	TerraformVersion string      `json:"terraform_version"`
	Values           StateValues `json:"values"`
}

func getVMState(id primitive.ObjectID) (*State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var runState map[string]interface{}
	filter := bson.M{
		"run": id.Hex(),
	}
	err := runStateCollection.FindOne(ctx, filter).Decode(&runState)
	if err != nil {
		return nil, fmt.Errorf("could not find any state")
	}

	var state State

	jsonErr := json.Unmarshal([]byte(runState["state"].(string)), &state)
	if jsonErr != nil {
		return nil, fmt.Errorf("could not read state %s", jsonErr)
	}
	return &state, nil

}

// Extra defines an accounting info (quantity and duration by resource name and kind)
type Extra struct {
	Name   string
	Kind   string
	Fields map[string]interface{}
}

// ExtraFields is an array of Extra
type ExtraFields struct {
	Extras []Extra
}

func getExtras(resourceType string, resourceValues map[string]interface{}) ExtraFields {
	extraFields := ExtraFields{}
	extraFields.Extras = make([]Extra, 0)

	switch resourceType {
	case "openstack_compute_instance_v2":
		if _, ok := resourceValues["block_device"]; ok {
			blocks := resourceValues["block_device"].([]interface{})
			for _, block := range blocks {
				destType := block.(map[string]interface{})["source_type"].(string)
				if destType == "blank" {
					size := block.(map[string]interface{})["volume_size"].(float64)
					fields := make(map[string]interface{})
					fields["quantity"] = size
					extra := Extra{
						Name:   "openstack_block_storage",
						Kind:   "block_device",
						Fields: fields,
					}
					extraFields.Extras = append(extraFields.Extras, extra)
				}
			}
		}
	}

	return extraFields

}

// Resource defined a vm, storage, ...
type Resource struct {
	Kind     string
	Resource string
	Quantity float64
}

// RunResource defines used resources by a run
type RunResource struct {
	Run       string
	Endpoint  string
	Resources []Resource
	Namespace string
}

func setAccounting(influxClient client.Client, run terraModel.Run, state *State, last int64, now int64) error {
	if last == 0 {
		last = run.Start
	}
	bp, err := client.NewBatchPoints(client.BatchPointsConfig{
		Database:  "goterra",
		Precision: "ns",
	})
	if err != nil {
		log.Error().Str("run", run.ID.Hex()).Msgf("failed to record stats %s", err)
		return fmt.Errorf("could not create stats")
	}

	usedResources := RunResource{
		Run:       run.ID.Hex(),
		Endpoint:  run.Endpoint,
		Resources: make([]Resource, 0),
		Namespace: run.Namespace,
	}

	acctExtras := make(map[string]Extra)
	ts := time.Unix(now, 0)
	if run.End > 0 {
		ts = time.Unix(run.End, 0)
	}

	for _, resource := range state.Values.RootModule.Resources {
		resourceType := resource["type"].(string)
		resourceName := resource["address"].(string)
		if !isManaged(resourceType) {
			continue
		}
		log.Info().Str("run", run.ID.Hex()).Msgf("Add accounting for %s", resourceName)

		resourceValues := resource["values"].(map[string]interface{})
		kind := "generic"

		fields := make(map[string]interface{})

		var extraFields ExtraFields

		switch resourceType {
		case "openstack_compute_instance_v2":
			kind = resourceValues["flavor_name"].(string)
			fields["quantity"] = 1.0
			fields["duration"] = float64(now - last)
			if run.End > 0 {
				fields["duration"] = float64(run.End - last)
			}
			extraFields = getExtras("openstack_compute_instance_v2", resourceValues)
		case "openstack_sharedfilesystem_share_v2":
			kind = "shared"
			fields["quantity"] = resourceValues["size"].(float64)
			fields["duration"] = float64(now - last)
			if run.End > 0 {
				fields["duration"] = float64(run.End - last)
			}
		}

		usedResources.Resources = append(usedResources.Resources, Resource{
			Kind:     kind,
			Resource: resourceType,
			Quantity: fields["quantity"].(float64),
		})

		uniqueID := fmt.Sprintf("%s-%s", resourceType, kind)
		if _, ok := acctExtras[uniqueID]; ok {
			log.Info().Msgf("Check %s", uniqueID)
			acctExtras[uniqueID].Fields["quantity"] = acctExtras[uniqueID].Fields["quantity"].(float64) + fields["quantity"].(float64)
		} else {
			acctExtras[uniqueID] = Extra{
				Name:   resourceType,
				Kind:   kind,
				Fields: fields,
			}
		}

		if len(extraFields.Extras) > 0 {

			duration := float64(now - last)
			if run.End > 0 {
				duration = float64(run.End - last)
			}

			for _, extra := range extraFields.Extras {
				uniqueID := fmt.Sprintf("%s-%s", extra.Name, extra.Kind)
				log.Info().Msgf("Check %s", uniqueID)

				if _, ok := acctExtras[uniqueID]; ok {
					acctExtras[uniqueID].Fields["quantity"] = acctExtras[uniqueID].Fields["quantity"].(float64) + fields["quantity"].(float64)
					acctExtras[uniqueID].Fields["duration"] = acctExtras[uniqueID].Fields["duration"].(float64) + (fields["quantity"].(float64) * duration)
				} else {
					extra.Fields["duration"] = extra.Fields["quantity"].(float64) * duration
					acctExtras[uniqueID] = extra
				}

				usedResources.Resources = append(usedResources.Resources, Resource{
					Kind:     extra.Kind,
					Resource: extra.Name,
					Quantity: extra.Fields["quantity"].(float64),
				})

			}
		}

	}

	if run.End != 0 {
		removeUsedResources(run.ID.Hex())
	} else {
		errResources := setUsedResources(usedResources)
		if errResources != nil {
			log.Error().Str("run", run.ID.Hex()).Msgf("failed to record resources %s", errResources)
		}
	}

	for _, extra := range acctExtras {
		tags := map[string]string{"endpoint": run.Endpoint, "resource": extra.Name, "ns": run.Namespace, "kind": extra.Kind}
		pt, err := client.NewPoint("goterra.acct", tags, extra.Fields, ts)
		if err != nil {
			log.Error().Str("run", run.ID.Hex()).Str("resource", extra.Name).Msgf("failed to record stats %s", err)
			continue
		}
		bp.AddPoint(pt)
	}

	// Write the batch
	if err := influxClient.Write(bp); err != nil {
		log.Error().Str("run", run.ID.Hex()).Msgf("failed to batch stats %s", err)
		return fmt.Errorf("could not create stats")
	}
	return nil
}

func removeUsedResources(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	filter := bson.M{
		"run": runID,
	}
	resourcesCollection.DeleteMany(ctx, filter)
}

func getUsedResources(ns string) []RunResource {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runResources := make([]RunResource, 0)
	filter := bson.M{}
	if ns != "" {
		filter["namespace"] = ns
	}
	cursor, err := resourcesCollection.Find(ctx, filter)
	if err != nil {
		log.Error().Msgf("failed to get resources %s", err)
		return runResources
	}
	for cursor.Next(ctx) {
		var runResource RunResource
		cursor.Decode(&runResource)
		log.Error().Msgf("add resource %+v", runResource)
		runResources = append(runResources, runResource)
	}
	return runResources
}

func setUsedResources(resources RunResource) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	filter := bson.M{
		"run": resources.Run,
	}
	var resourcedb RunResource
	resourceErr := resourcesCollection.FindOne(ctx, filter).Decode(&resourcedb)
	if resourceErr == nil {
		// We already recorded used resources, skipping
		log.Debug().Str("run", resources.Run).Msgf("resources already recorded, skipping")
		return nil
	}

	newRunResource, resourceErr := resourcesCollection.InsertOne(ctx, resources)
	if resourceErr != nil {
		log.Error().Str("run", resources.Run).Msgf("Failed to insert run resources: %s", resourceErr)
		return resourceErr
	}
	log.Debug().Str("Resources", newRunResource.InsertedID.(primitive.ObjectID).Hex()).Msg("New run resource inserted")
	return nil
}

func setLastCheck(newCheck int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	filter := bson.M{
		"id": 1,
	}
	newCheckInfo := bson.M{
		"$set": bson.M{
			"last_check": newCheck,
		},
	}
	upsert := true
	options := &mongoOptions.FindOneAndUpdateOptions{
		Upsert: &upsert,
	}

	acctCollection.FindOneAndUpdate(ctx, filter, newCheckInfo, options)
}

func fetchAcct(lastCheck int64) {

	c, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     configAcct.InfluxDB.URL,
		Username: configAcct.InfluxDB.User,
		Password: configAcct.InfluxDB.Password,
	})
	if err != nil {
		log.Error().Msgf("%s", err)
		os.Exit(1)
	}
	defer c.Close()

	for true {
		log.Info().Msg("Start accounting")
		now := time.Now().Unix()
		/*
			redisClient := redis.NewClient(&redis.Options{
				Addr:     "localhost:6379",
				Password: "", // no password set
				DB:       0,  // use default DB
			})*/
		gotError := false
		runs := getLastVM(now, lastCheck)
		log.Info().Msgf("Update %d VMs", len(runs))
		for _, run := range runs {
			log.Debug().Msgf("Run: %+v", run)
			state, stateErr := getVMState(run.ID)
			if stateErr != nil {
				gotError = true
				log.Error().Msgf("Failed to get state: %s", stateErr)
				continue
			}
			setAccounting(c, run, state, lastCheck, now)
		}
		if !gotError {
			setLastCheck(now)
		}
		//redisClient.Close()
		time.Sleep(1 * time.Minute)
	}
}

func main() {

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if os.Getenv("GOT_DEBUG") != "" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	config := terraConfig.LoadConfig()

	configFile := "acct.yml"
	if os.Getenv("GOT_ACCT_CONFIG") != "" {
		configFile = os.Getenv("GOT_ACCT_CONFIG")
	}
	cfgfile, _ := ioutil.ReadFile(configFile)
	configAcct = AcctConfig{}
	yamlErr := yaml.Unmarshal([]byte(cfgfile), &configAcct)
	if yamlErr != nil {
		log.Error().Msgf("failed to load acct config file: %s, %s", configFile, yamlErr)
	}
	if os.Getenv("GOT_INFLUX_URL") != "" {
		configAcct.InfluxDB.URL = os.Getenv("GOT_INFLUX_URL")
	}
	if os.Getenv("GOT_INFLUX_USER") != "" {
		configAcct.InfluxDB.User = os.Getenv("GOT_INFLUX_USER")
	}
	if os.Getenv("GOT_INFLUX_PASSWORD") != "" {
		configAcct.InfluxDB.Password = os.Getenv("GOT_INFLUX_PASSWORD")
	}

	if os.Getenv("GOT_INFLUX_RESOURCES") != "" {
		configAcct.Resources = strings.Split(os.Getenv("GOT_INFLUX_RESOURCES"), ",")
	}

	consulErr := terraConfig.ConsulDeclare("got-acct", "/acct")
	if consulErr != nil {
		log.Error().Msgf("Failed to register: %s", consulErr.Error())
		panic(consulErr)
	}

	mongoClient, err := mongo.NewClient(mongoOptions.Client().ApplyURI(config.Mongo.URL))
	if err != nil {
		log.Error().Msgf("Failed to connect to mongo server %s\n", config.Mongo.URL)
		os.Exit(1)
	}
	ctx, cancelMongo := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMongo()

	err = mongoClient.Connect(ctx)
	if err != nil {
		log.Error().Msgf("Failed to connect to mongo server %s\n", config.Mongo.URL)
		os.Exit(1)
	}
	nsCollection = mongoClient.Database(config.Mongo.DB).Collection("ns")
	runCollection = mongoClient.Database(config.Mongo.DB).Collection("run")
	runStateCollection = mongoClient.Database(config.Mongo.DB).Collection("runstate")
	acctCollection = mongoClient.Database(config.Mongo.DB).Collection("acct")
	resourcesCollection = mongoClient.Database(config.Mongo.DB).Collection("acctresources")

	// userCollection = mongoClient.Database(config.Mongo.DB).Collection("users")
	var acctCheck AcctCheck
	filter := bson.M{
		"id": 1,
	}
	var lastCheck int64
	errLastCheck := acctCollection.FindOne(ctx, filter).Decode(&acctCheck)
	if errLastCheck != nil {
		lastCheck = 0
	} else {
		lastCheck = acctCheck.LastCheck
	}
	go fetchAcct(lastCheck)

	r := mux.NewRouter()
	r.HandleFunc("/acct", HomeHandler).Methods("GET")
	r.HandleFunc("/acct/resources", ResourceHandler).Methods("GET")
	r.HandleFunc("/acct/resources/ns/{id}", ResourceNSHandler).Methods("GET")
	r.HandleFunc("/acct/ns/{id}", AcctGetHandler).Methods("GET")
	r.HandleFunc("/acct/ns/{id}/from/{from}", AcctGetFromHandler).Methods("GET")
	r.HandleFunc("/acct/ns/{id}/from/{from}/to/{to}", AcctGetFromToHandler).Methods("GET")

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE"},
	})
	handler := c.Handler(r)

	loggedRouter := handlers.LoggingHandler(os.Stdout, handler)

	srv := &http.Server{
		Handler: loggedRouter,
		Addr:    fmt.Sprintf("%s:%d", config.Web.Listen, config.Web.Port),
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	srv.ListenAndServe()

}
