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
        "strconv"

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
        Key string
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

        r.GET("/consulta/:citizenID/:birthdate", func(c *gin.Context) {
                citizenID := strings.ToUpper(c.Param("citizenID"))
                birthdate := c.Param("birthdate")

		// Fix birthday with just 1 char
		if len(birthdate) == 1 {
			birthdate = "0" + birthdate
		}

                citizenInfo, err := getCitizenFromDB(CitizenKey{citizenID+birthdate})
                if err != nil {
                        if err == sql.ErrNoRows {
                                c.JSON(http.StatusNotFound, gin.H{"errorMessage": "Not found"})
                        } else {
                                c.JSON(http.StatusInternalServerError, gin.H{"errorMessage": "Internal server error"})
                        }
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

        return nil
}

func initDB(dbPath string) (*sql.DB, error) {
        var err error
        db, err := sql.Open("sqlite3", dbPath)

        if err != nil {
                return nil, fmt.Errorf("Error opening database: %v", err)
        }

        createCitizensTableQuery := `
                CREATE TABLE IF NOT EXISTS citizens (
                        citizen_key TEXT PRIMARY KEY,
                        colele TEXT NOT NULL,
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
        // Get number of document and birthdate chars from ENV or defaults
        documentChars := 5
        if envDocumentChars := os.Getenv("DOCUMENT_CHARS"); envDocumentChars != "" {
          documentChars, _ = strconv.Atoi(envDocumentChars)
        }
        //birthDateChars := 2
        /* Supressed, random python isn't great and generates artificial collisions
        in test db. Dixed  dd+yy as elements in the key so we get the biggest comb
        space to avoid collisions ( ~ 1500 high-freq entries out of 10k comb )
        if envBirthDateChars := os.Getenv("BIRTHDATE_CHARS"); envBirthDateChars != "" {
          birthDateChars, _ = strconv.Atoi(envBirthDateChars)
        }
        */


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

        insertCitizenStmt, err := tx.Prepare(`INSERT INTO citizens (citizen_key, colele) VALUES (?, ?)`)
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
//        	} else {
//                        log.Printf("Full record: %s - %s\n", record[25], record[27])
        	}


                citizenID := strings.ToUpper(record[27])
                birthdate := record[25]
                dircol := strings.Join([]string{record[9], record[10], record[11], record[12]}, " ")


                // Shall we get a substring of CitizenID?
                if envDocumentChars, _ := strconv.ParseInt(os.Getenv("DOCUMENT_CHARS"), 10, 8); envDocumentChars >= 4 {
        		// Store only N characters of the citizenID. FIRST==true; default last
        		if envFirst, _ := strconv.ParseBool(os.Getenv("FIRST")); envFirst == true {
        			citizenID = citizenID[:documentChars]
        		} else {
        			citizenID = citizenID[len(citizenID)-documentChars:]
        		}
		} else if envDocumentChars == 0 {
                        
		} else {
                        return fmt.Errorf("Error, documentChars is smaller than 4, aborting ")

		}


                // Allow to add the lasting 2 digits of the year in case the census has collisions
                if envYear, _ := strconv.ParseBool(os.Getenv("YEAR")); envYear == true {
                        birthdate = birthdate[:2] + birthdate[len(birthdate)-2:]
                } else {
                        birthdate = birthdate[:2]
                }
         

                citizenKey := citizenID + birthdate
                log.Printf("KEY: %s\n", citizenKey)

                lmun, _ := decoder.String(record[2])
                dist := record[3]
                secc := record[4]
                mesa := record[5]
                nlocal, _ := decoder.String(record[6])

                _, err = insertCitizenStmt.Exec(citizenKey, nlocal)
                if err != nil {
                        return fmt.Errorf("Error inserting citizen: %v - %v - %v", err, citizenID, birthdate)
                }
                rowsImported++

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

func getCitizenFromDB(key CitizenKey) (CitizenInfo, error) {
        citizenInfo := CitizenInfo{}

        query := `
                SELECT
                        c.citizen_key, p.poblacion, p.distrito, p.seccion, p.mesa, p.id, p.dircol
                FROM
                        citizens c
                JOIN
                        polling_stations p ON c.colele = p.id
                WHERE
                        c.citizen_key = ?
        `
        err := db.QueryRow(query, key.Key).Scan(
                &key.Key,
                &citizenInfo.Poblacion, &citizenInfo.Distrito, &citizenInfo.Seccion,
                &citizenInfo.Mesa, &citizenInfo.Colele, &citizenInfo.Dircol,
        )

        if err != nil {
                return citizenInfo, err
        }

        return citizenInfo, nil
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

