// vim: set ts=4 sw=4 noet:
package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"path/filepath"
	"sync"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/text/encoding/charmap"
)

var location *time.Location
var timeFormat string
var db *sql.DB
var dbReady = false
var dbReadyMutex sync.Mutex

type CitizenInfo struct {
	Poblacion    string `json:"poblacion"`
	Distrito     string `json:"distrito"`
	Seccion      string `json:"seccion"`
	Mesa         string `json:"mesa"`
	Colele       string `json:"colele"`
	Dircol       string `json:"dircol"`
	ErrorMessage string `json:"errorMessage"`
}

type CitizenKey struct {
	CitizenID    string
	Day          string
	Year         string
	Fn           string
	Sn1          string
	Sn2          string
	PostCode     string
	Colele       string
}

type RequestData struct {
	CitizenID    string `json:"citizenId" binding:"required"`
	Day          string `json:"day"`
	Year         string `json:"year"`
	Fn           string `json:"fn"`
	Sn1          string `json:"sn1"`
	Sn2          string `json:"sn2"`
	PostCode     string `json:"postCode`
	Colele       string `json:"colele`
}

type ComboResult struct {
	Combo      []string
	Percentage float64
}

func main() {

	files, err := os.ReadDir("/data")
	if err != nil {
		log.Fatalf("Error reading data directory: %v", err)
	}

	var csvFilePath, dbFilePath string

	for _, file := range files {
		if strings.ToLower(filepath.Ext(file.Name())) == ".csv" || strings.ToLower(filepath.Ext(file.Name())) == ".txt" {
			csvFilePath = filepath.Join("/data", file.Name())
		} else if strings.ToLower(filepath.Ext(file.Name())) == ".db" {
			dbFilePath = filepath.Join("/data", file.Name())
		}
	}

	if dbFilePath == "" && csvFilePath == "" {
		log.Fatalf("No database or CSV file found in the data directory")
	}

	if dbFilePath == "" {
		dbFilePath = "/data/citizens.db"
	}

	if csvFilePath != "" {
		// Check if the DB file exists
		if _, err := os.Stat(dbFilePath); err == nil {
			// Delete the DB file
			err = os.Remove(dbFilePath)
			if err != nil {
				fmt.Errorf("Error deleting DB file: %v", err)
				return
			}
		}
		db, err = initDB(dbFilePath) // Assign the returned *sql.DB to the global db variable

		if err != nil {
			log.Fatalf("Error initializing database: %v", err)
		}
		defer db.Close()

		log.Println("Detected CSV file. Starting to parse...")
		start := time.Now()
		err = loadCitizens(csvFilePath, dbFilePath)
		if err != nil {
			log.Fatalf("Error loading citizens from CSV: %v", err)
		}
		log.Printf("Citizens loaded in %v\n", time.Since(start))

		if _, err := os.Stat(csvFilePath); err == nil {
			err = os.Remove(csvFilePath)
			if err != nil {
				log.Printf("Error removing CSV file: %v", err)
			}
		}
	} else {

		db, err = initDB(dbFilePath) // Assign the returned *sql.DB to the global db variable

		if err != nil {
			log.Fatalf("Error initializing database: %v", err)
		}
		defer db.Close()

	}

	dbReadyMutex.Lock()
	dbReady = true
	dbReadyMutex.Unlock()

	r := gin.New()
	r.Use(customLogger(), TokenAuthMiddleware(), gin.Recovery())

	r.POST("/consulta", func(c *gin.Context) {
		var requestData RequestData
		if err := c.ShouldBindJSON(&requestData); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	   
		citizenID := strings.ToUpper(requestData.CitizenID)
		day := requestData.Day
		year := requestData.Year
		fn := strings.ToUpper(requestData.Fn)
		sn1 := strings.ToUpper(requestData.Sn1)
		sn2 := strings.ToUpper(requestData.Sn2)
		postCode := strings.ToUpper(requestData.PostCode)
		colele := strings.ToUpper(requestData.Colele)
 
		// Fix day with just 1 char
		if len(day) == 1 {
			day = "0" + day
		}
  
		key := CitizenKey{
			CitizenID: citizenID,
			Day:	   day,
			Year:	   year,
			Fn:        fn,
			Sn1:	   sn1,
			Sn2:	   sn2,
			PostCode:  postCode,
			Colele:    colele,
		}
		
		citizenInfo, numResults, err := getCitizenFromDB(key)
		if err != nil {
//			c.JSON(http.StatusInternalServerError, gin.H{"errorMessage": err.Error()})
			c.JSON(http.StatusOK, gin.H{"errorMessage": err.Error()})
			return
		}
		
		if numResults > 1 {
//			c.JSON(http.StatusBadRequest, gin.H{"errorMessage": err})
			c.JSON(http.StatusOK, gin.H{"errorMessage": err})
			return
		}
		
		c.JSON(http.StatusOK, citizenInfo)
	})

	r.Run()
}

func isDBReady() bool {
	dbReadyMutex.Lock()
	defer dbReadyMutex.Unlock()
	return dbReady
}

func loadCitizens(filePath, dbPath string) (error) {
	// Initialize the database
	db, err := initDB(dbPath)

	if err != nil {
		return err
	}

	if db == nil {
		return nil
	}

	// Check if the CSV file exists
	if _, err := os.Stat(filePath); err == nil {
		err = loadCitizensFromCSV(filePath)
		if err != nil {
			return err
		}

		// Delete the CSV file
		err = os.Remove(filePath)
		if err != nil {
			return fmt.Errorf("Error deleting CSV file: %v", err)
		}
	}

	// Show statistics on screen
	results, err := calculateUniquePercentages(db)
	if err != nil {
		log.Fatalf("Error calculating unique percentages: %v\n", err)
	}
	printResults(results)	

	return nil
}

func calculateUniquePercentages(db *sql.DB) ([]ComboResult, error) {
	combinations := [][]string{
		{},
		{"day"},
		{"year"},
		{"fn"},
		{"sn1"},
		{"sn2"},
		{"postCode"},
		{"colele"},
	}

	uniqueCounts := make(map[string]int)
	for _, combo := range combinations {
		fields := append([]string{"citizen_id"}, combo...)
		query := fmt.Sprintf("SELECT DISTINCT %s FROM citizens", strings.Join(fields, ", "))
		rows, err := db.Query(query)
		if err != nil {
			return nil, err
		}

		count := 0
		for rows.Next() {
			count++
		}
		uniqueCounts[strings.Join(combo, "")] = count
	}

	var totalRows int
	err := db.QueryRow("SELECT COUNT(*) FROM citizens").Scan(&totalRows)
	if err != nil {
		return nil, err
	}

	results := make([]ComboResult, 0, len(combinations))
	for _, combo := range combinations {
		count := uniqueCounts[strings.Join(combo, "")]
		percentage := float64(count) / float64(totalRows) * 100.0
		results = append(results, ComboResult{Combo: combo, Percentage: percentage})
	}

	return results, nil
}

func printResults(results []ComboResult) {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Percentage > results[j].Percentage
	})

	for _, result := range results {
		var fields string
		if len(result.Combo) > 0 {
			fields = "citizen_id+" + strings.Join(result.Combo, "+")
		} else {
			fields = "citizen_id"
		}
		log.Printf("%s = %.2f%%\n", fields, result.Percentage)
	}
}

func initDB(dbPath string) (*sql.DB, error) {
	var err error
	db, err := sql.Open("sqlite3", dbPath)

	if err != nil {
		return nil, fmt.Errorf("Error opening database: %v", err)
	}

	createCitizensTableQuery := `
		CREATE TABLE IF NOT EXISTS citizens (
			citizen_id TEXT NOT NULL,
			day TEXT NOT NULL,
			year TEXT NOT NULL,
			fn TEXT NOT NULL,
			sn1 TEXT NOT NULL,
			sn2 TEXT NOT NULL,
			postCode TEXT NOT NULL,
			colele TEXT NOT NULL,
			PRIMARY KEY (citizen_id, day, year, fn, sn1, sn2),
			FOREIGN KEY (colele) REFERENCES polling_stations (id)
		)
	`
	createPollingStationsTableQuery := `
		CREATE TABLE IF NOT EXISTS polling_stations (
			id TEXT PRIMARY KEY,
			poblacion TEXT,
			distrito TEXT,
			seccion TEXT,
			mesa TEXT,
			dircol TEXT
		)
	`

	_, err = db.Exec(createCitizensTableQuery)
	if err != nil {
		return nil, fmt.Errorf("Error creating citizens table: %v", err)

	}

	_, err = db.Exec(createPollingStationsTableQuery)
	if err != nil {
		return nil, fmt.Errorf("Error creating polling stations table: %v", err)
	}

	return db, nil

}

func loadCitizensFromCSV(filePath string) error {

	nameChars := 2
	if envNameChars, _ := strconv.ParseInt(os.Getenv("NAME_CHARS"), 10, 8); envNameChars >= 1 {
			nameChars = int(envNameChars)
	}
   

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("Error opening file: %v", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.Comma = ';'
	reader.FieldsPerRecord = -1

	decoder := charmap.ISO8859_1.NewDecoder()

	// Read and discard the header line
	_, err = reader.Read()
	if err != nil {
		return fmt.Errorf("Error reading CSV header: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("Error starting transaction: %v", err)
	}
	defer tx.Rollback()

	insertCitizenStmt, err := tx.Prepare(`INSERT INTO citizens (citizen_id, day, year, fn, sn1, sn2, postCode, colele) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("Error preparing citizen insert statement: %v", err)
	}
	defer insertCitizenStmt.Close()

	insertPollingStationStmt, err := tx.Prepare(`INSERT OR IGNORE INTO polling_stations (id, poblacion, distrito, seccion, mesa, dircol) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("Error preparing polling station insert statement: %v", err)
	}
	defer insertPollingStationStmt.Close()

	var rowsRead, rowsImported int
	//var day, year, sn1, sn2 string

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("Error reading CSV file: %v", err)
		}

		rowsRead++

		if record[27] == "" || record[25] == "" {
			log.Printf("Empty record: %s - %s\n", record[25], record[27])
			continue
		}

		citizenID := strings.ToUpper(record[27])
		birthdate := record[25]
		dircol := strings.Join([]string{record[9], record[10], record[11], record[12]}, " ")

		// Shall we get a substring of CitizenID?
		if envDocumentChars, _ := strconv.ParseInt(os.Getenv("DOCUMENT_CHARS"), 10, 8); envDocumentChars >= 3 {
			// Store only N characters of the citizenID. FIRST==true; default last
			if envFirst, _ := strconv.ParseBool(os.Getenv("FIRST_CHARS")); envFirst == true {
				if envLetter, _ := strconv.ParseBool(os.Getenv("FIRST_CHARS_ADD_LETTER")); envLetter == true {
					citizenID = citizenID[:envDocumentChars]+citizenID[len(citizenID)-1:]
				} else {
					citizenID = citizenID[:envDocumentChars]
			}
			} else {
				citizenID = citizenID[len(citizenID)-int(envDocumentChars):]
			}
		} else if envDocumentChars == 0 {
			
		} else {
			return fmt.Errorf("Error, documentChars is smaller than 3, aborting ")

		}

		// Additional indexes will be empty if not enabled in the environment
		fn := ""
		if envFn, _ := strconv.ParseBool(os.Getenv("FN")); envFn == true {
			fnDecoded, _ := decoder.String(record[13])
			fn = fnDecoded
			if len(fn) > nameChars {
				fn = truncateUTF8String(fn, nameChars)
			}
		}

		sn1 := ""
		if envSn1, _ := strconv.ParseBool(os.Getenv("SN1")); envSn1 == true {
			sn1Decoded, _ := decoder.String(record[14])
			sn1 = sn1Decoded
			if len(sn1) >= nameChars  {
				sn1 = truncateUTF8String(sn1, nameChars)
			}
		}

		sn2 := ""
		if envSn2, _ := strconv.ParseBool(os.Getenv("SN2")); envSn2 == true {
			sn2Decoded, _ := decoder.String(record[15])
			sn2 = sn2Decoded
			if len(sn2) > nameChars {
				sn2 = truncateUTF8String(sn2, nameChars)
			}
		}

		day := ""
		if envDay, _ := strconv.ParseBool(os.Getenv("DAY")); envDay == true {
			day = birthdate[:2]
		}

		year := ""
		if envYear, _ := strconv.ParseBool(os.Getenv("YEAR")); envYear == true {
			year = birthdate[len(birthdate)-2:]
		}

		postCode := ""
		if envPostCode, _ := strconv.ParseBool(os.Getenv("POST_CODE")); envPostCode == true {
			postCode = record[28]
		}

		lmun, _ := decoder.String(record[2])
		dist := record[3]
		secc := record[4]
		mesa := record[5]
		nlocal, _ := decoder.String(record[6])

		_, err = insertCitizenStmt.Exec(citizenID, day, year, fn, sn1, sn2, postCode, nlocal)
		if err != nil {
			return fmt.Errorf("Error inserting citizen: %v - %v - %v - %v - %v - %v - %v - %v", err, citizenID, day, year, fn, sn1, sn2, postCode)
		}
		rowsImported++

//	        log.Printf("Decoded record: %s - %s - %s\n", fn, sn1, sn2)

		_, err = insertPollingStationStmt.Exec(nlocal, lmun, dist, secc, mesa, strings.TrimSpace(dircol))
		if err != nil {
			return fmt.Errorf("Error inserting polling station: %v", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("Error committing transaction: %v", err)
	}

	log.Printf("CSV import process: %d rows read, %d rows imported\n", rowsRead, rowsImported)
	return nil
}

func getCitizenFromDB(key CitizenKey) (CitizenInfo, int, error) {
	citizenInfo := CitizenInfo{}

	query := `
		SELECT
			c.citizen_id, c.day, c.year, c.fn, c.sn1, c.sn2, c.postCode, c.colele
		FROM
			citizens c
		WHERE
			c.citizen_id = ?`
	
	args := []interface{}{key.CitizenID}
	if key.Day != "" {
		query += " AND c.day = ?"
		args = append(args, key.Day)
	}
	if key.Year != "" {
		query += " AND c.year = ?"
		args = append(args, key.Year)
	}
	if key.Fn != "" {
		query += " AND c.fn = ?"
		args = append(args, key.Fn)
	}
	if key.Sn1 != "" {
		query += " AND c.sn1 = ?"
		args = append(args, key.Sn1)
	}
	if key.Sn2 != "" {
		query += " AND c.sn2 = ?"
		args = append(args, key.Sn2)
	}
	if key.PostCode != "" {
		query += " AND c.postCode = ?"
		args = append(args, key.PostCode)
	}
	if key.Colele != "" {
		query += " AND c.colele = ?"
		args = append(args, key.Colele)
	}


	rows, err := db.Query(query, args...)

	if err != nil {
		return citizenInfo, 0, err
	}
	defer rows.Close()

	var results []CitizenKey
	for rows.Next() {
	var result CitizenKey
	err = rows.Scan(
		&result.CitizenID, &result.Day, &result.Year, &result.Fn, &result.Sn1, &result.Sn2, &result.PostCode, &result.Colele,
	)

	if err != nil {
		return citizenInfo, 0, err
	}
		results = append(results, result)
	}

	if len(results) == 0 {
		return citizenInfo, len(results), fmt.Errorf("No match found")
	} else if len(results) > 1 {
		diffFields := findDifferingFieldsCitizens(results)
		return citizenInfo, len(results), fmt.Errorf("%v", diffFields)
	}

	// Query for the CitizenInfo using the unique CitizenKey
	if len(results) == 1 {
		citizenID := results[0].CitizenID
		day       := results[0].Day
		year      := results[0].Year
		fn        := results[0].Fn
		sn1       := results[0].Sn1
		sn2       := results[0].Sn2
		postCode  := results[0].PostCode

		infoQuery := `
			SELECT
				p.poblacion, p.distrito, p.seccion, p.mesa, p.dircol, c.colele
			FROM
				citizens c
				JOIN polling_stations p ON c.colele = p.id
			WHERE
				c.citizen_id = ? AND c.day = ? AND c.year = ? AND c.fn = ? AND c.sn1 = ? AND c.sn2 = ? AND c.postCode = ?
		`

		err = db.QueryRow(infoQuery, citizenID, day, year, fn, sn1, sn2, postCode).Scan(
			&citizenInfo.Poblacion, &citizenInfo.Distrito, &citizenInfo.Seccion,
			&citizenInfo.Mesa, &citizenInfo.Dircol, &citizenInfo.Colele,
		)

		if err != nil {
			return citizenInfo, len(results), err
		}
	}

	// len(results) might be 1 and shoudn't reach this point
	return citizenInfo, len(results), err
}

func findDifferingFieldsCitizens(results []CitizenKey) []string {

	if len(results) < 2 {
		return nil
	}

	differingFields := []string{}
	first := results[0]
	for _, result := range results[1:] {
		if first.CitizenID != result.CitizenID && !contains(differingFields, "citizen_id") {
			differingFields = append(differingFields, "citizen_id")
		}
		if first.Day != result.Day && !contains(differingFields, "day") {
			differingFields = append(differingFields, "day")
		}
		if first.Year != result.Year && !contains(differingFields, "year") {
			differingFields = append(differingFields, "year")
		}
		if first.Fn != result.Fn && !contains(differingFields, "fn") {
			differingFields = append(differingFields, "fn")
		}
		if first.Sn1 != result.Sn1 && !contains(differingFields, "sn1") {
			differingFields = append(differingFields, "sn1")
		}
		if first.Sn2 != result.Sn2 && !contains(differingFields, "sn2") {
			differingFields = append(differingFields, "sn2")
		}
		if first.PostCode != result.PostCode && !contains(differingFields, "postCode") {
			differingFields = append(differingFields, "postCode")
		}
		if first.PostCode != result.PostCode && !contains(differingFields, "colele") {
			differingFields = append(differingFields, "colele")
		}
	}

	return differingFields
}

func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

func init() {
	gin.SetMode(gin.ReleaseMode)

	timezone := os.Getenv("TIMEZONE")
	if timezone == "" {
		timezone = "UTC"
	}

	var err error
	location, err = time.LoadLocation(timezone)
	if err != nil {
		log.Fatalf("Error loading timezone: %v", err)
	}

	timeFormat = "02/Jan/2006:15:04:05 -0700"

	r := gin.New()
	r.Use(customLogger(), gin.Recovery())
	r.GET("/health", func(c *gin.Context) {
		if isDBReady() {
			c.Status(http.StatusOK)
		} else {
			c.Status(http.StatusServiceUnavailable)
		}
	})
}

func customLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		end := time.Now()
		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()

		log.Printf("%s - - [%s] \"%s %s %s\" %d %d \"%s\" \"%s\"\n",
			clientIP,
			end.In(location).Format(timeFormat),
			method,
			c.Request.URL.Path,
			c.Request.Proto,
			statusCode,
			c.Writer.Size(),
			c.Request.Referer(),
			c.Request.UserAgent(),
		)
	}
}

func TokenAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.Request.Header.Get("Authorization")
		envToken := os.Getenv("TOKEN")

		if token == "" || token != envToken {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error": "Unauthorized",
				})
				return
		}

		c.Next()
	}
}

func truncateUTF8String(s string, n int) string {
    if utf8.RuneCountInString(s) <= n {
        return s
    }

    truncated := make([]rune, n)
    i := 0

    for _, r := range s {
        if i >= n {
            break
        }

        truncated[i] = r
        i++
    }

    return string(truncated)
}
