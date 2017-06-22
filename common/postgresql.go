package common

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx"
	"golang.org/x/crypto/bcrypt"
)

var (
	// PostgreSQL connection pool handle
	pdb *pgx.ConnPool
)

// Add a user to the system.
func AddUser(auth0ID string, userName string, password string, email string, displayName string) error {
	// Hash the user's password
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Failed to hash user password. User: '%v', error: %v.\n", userName, err)
		return err
	}

	// Generate a unique bucket name for the user
	var bucket string
	newBucket := true
	for newBucket == true {
		bucket = RandomString(16) + ".bkt"
		newBucket, err = MinioBucketExists(bucket) // Drops out of the loop when the name hasn't been used yet
		if err != nil {
			return err
		}
	}

	// Generate a new HTTPS client certificate for the user
	cert, err := GenerateClientCert(userName, 14) // 14 days validity while developing
	if err != nil {
		log.Printf("Error when generating client certificate for '%s': %v\n", userName, err)
		return err
	}

	// If the displayName variable is an empty string, we insert a NULL instead
	var dn pgx.NullString
	if displayName == "" {
		dn.Valid = false
	} else {
		dn.String = displayName
		dn.Valid = true
	}

	// Add the new user to the database
	insertQuery := `
		INSERT INTO users (auth0_id, user_name, email, password_hash, client_cert, display_name)
		VALUES ($1, $2, $3, $4, $5, $6)`
	commandTag, err := pdb.Exec(insertQuery, auth0ID, userName, email, hash, cert, dn)
	if err != nil {
		log.Printf("Adding user to database failed: %v\n", err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows affected when creating user: %v, username: %v\n", numRows, userName)
	}

	// Create a new bucket for the user in Minio
	err = CreateMinioBucket(bucket)
	if err != nil {
		log.Printf("Error creating new bucket: %v\n", err)
		return err
	}

	// Log the user registration
	log.Printf("User registered: '%s' Email: '%s'\n", userName, email)

	return nil
}

// Add a new SQLite database for a user.
func AddDatabase(dbOwner string, dbFolder string, dbName string, dbVer int, shaSum []byte, dbSize int, public bool, bucket string, id string, descrip string, readme string) error {
	// Check for values which should be NULL
	var nullableDescrip, nullableReadme pgx.NullString
	if descrip == "" {
		nullableDescrip.Valid = false
	} else {
		nullableDescrip.String = descrip
		nullableDescrip.Valid = true
	}
	if readme == "" {
		nullableReadme.Valid = false
	} else {
		nullableReadme.String = readme
		nullableReadme.Valid = true
	}

	// If it's a new database, add its details to the main PG sqlite_databases table
	var dbQuery string
	if dbVer == 1 {
		dbQuery = `
			WITH root_db_value AS (
				SELECT nextval('sqlite_databases_idnum_seq')
			)
			INSERT INTO sqlite_databases (user_id, folder, dbname, public, db_id, root_database, description, readme)
			VALUES ($1, $2, $3, $4, (SELECT nextval FROM root_db_value), $5, (SELECT nextval FROM root_db_value), $6, $7)`
		commandTag, err := pdb.Exec(dbQuery, dbOwner, dbFolder, dbName, public, bucket, nullableDescrip,
			nullableReadme)
		if err != nil {
			log.Printf("Adding database to PostgreSQL failed: %v\n", err)
			return err
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf("Wrong number of rows (%v) affected when creating initial sqlite_databases "+
				"entry for '%s%s/%s'\n", numRows, dbOwner, dbFolder, dbName)
		}
	}

	// Add the database to database_versions
	dbQuery = `
		WITH databaseid AS (
			SELECT db_id
			FROM sqlite_databases
			WHERE user_id = $1
				AND dbname = $2)
		INSERT INTO database_versions (db, size, version, sha256, minioid)
		SELECT idnum, $3, $4, $5, $6
		FROM databaseid`
	commandTag, err := pdb.Exec(dbQuery, dbOwner, dbName, dbSize, dbVer, hex.EncodeToString(shaSum[:]), id)
	if err != nil {
		log.Printf("Adding version info to PostgreSQL failed: %v\n", err)
		return err
	}

	// Update the last_modified date for the database in sqlite_databases
	dbQuery = `
		UPDATE sqlite_databases
		SET last_modified = (
			SELECT last_modified
			FROM database_versions
			WHERE db = (
				SELECT idnum
				FROM sqlite_databases
				WHERE username = $1
					AND dbname = $2)
				AND version = $3)
		WHERE username = $1
			AND dbname = $2`
	commandTag, err = pdb.Exec(dbQuery, dbOwner, dbName, dbVer)
	if err != nil {
		log.Printf("Updating last_modified date in PostgreSQL failed: %v\n", err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows affected: %v, user: %s, database: %v\n", numRows, dbOwner, dbName)
	}

	return nil
}

// Check if a database exists
// If an error occurred, the true/false value should be ignored, as only the error value is valid.
func CheckDBExists(dbOwner string, dbFolder string, dbName string) (bool, error) {
	dbQuery := `
		SELECT count(db_id)
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	var DBCount int
	err := pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&DBCount)
	if err != nil {
		log.Printf("Checking if a database exists failed: %v\n", err)
		return true, err
	}
	if DBCount == 0 {
		// Database isn't in our system
		return false, nil
	}

	// Database exists
	return true, nil
}

// Check if a database has been starred by a given user.  The boolean return value is only valid when err is nil.
func CheckDBStarred(loggedInUser string, dbOwner string, dbFolder string, dbName string) (bool, error) {
	dbQuery := `
		SELECT count(db_id)
		FROM database_stars
		WHERE database_stars.user_id = (
			SELECT user_id
			FROM users
			WHERE user_name = $1)
		AND database_stars.db_id = (
			SELECT db_id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $2)
				AND folder = $3
				AND db_name = $4)`
	var starCount int
	err := pdb.QueryRow(dbQuery, loggedInUser, dbOwner, dbFolder, dbName).Scan(&starCount)
	if err != nil {
		log.Printf("Error looking up star count for database. User: '%s' DB: '%s/%s'. Error: %v\n",
			loggedInUser, dbOwner, dbName, err)
		return true, err
	}
	if starCount == 0 {
		// Database hasn't been starred by the user
		return false, nil
	}

	// Database HAS been starred by the user
	return true, nil
}

// Check if an email address already exists in our system. Returns true if the email is already in the system, false
// if not.  If an error occurred, the true/false value should be ignored, as only the error value is valid.
func CheckEmailExists(email string) (bool, error) {
	// Check if the email address is already in our system
	dbQuery := `
		SELECT count(user_name)
		FROM users
		WHERE email = $1`
	var emailCount int
	err := pdb.QueryRow(dbQuery, email).Scan(&emailCount)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return true, err
	}
	if emailCount == 0 {
		// Email address isn't yet in our system
		return false, nil
	}

	// Email address IS already in our system
	return true, nil

}

// Checks if a given MinioID string is available for use by a user. Returns true if available, false if not.  Only
// if err returns a non-nil value.
func CheckMinioIDAvail(userName string, id string) (bool, error) {
	// Check if an existing database for the user already uses the given MinioID
	var dbVer int
	dbQuery := `
		WITH user_databases AS (
			SELECT idnum
			FROM sqlite_databases
			WHERE username = $1)
		SELECT ver.version
		FROM database_versions AS ver, user_databases AS db
		WHERE ver.db = db.idnum
			AND ver.minioid = $2`
	err := pdb.QueryRow(dbQuery, userName, id).Scan(&dbVer)
	if err != nil {
		if err == pgx.ErrNoRows {
			// Not a real error, there just wasn't a matching row
			return true, nil
		}

		// A real database error occurred
		log.Printf("Error checking if a MinioID is already taken: %v\n", err)
		return false, err
	}

	if dbVer == 0 {
		// Nothing already using the MinioID, so it's available for use
		return true, nil
	}

	// The MinioID is already in use
	return false, nil
}

// Check if a user has access to a database.
// Returns true if it's accessible to them, false if not.  If err returns as non-nil, the true/false value isn't valid.
func CheckUserDBAccess(dbOwner string, dbFolder string, dbName string, loggedInUser string) (bool, error) {
	dbQuery := `
		SELECT count(*)
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	if dbOwner != loggedInUser {
		dbQuery += ` AND public = true `
	}
	var numRows int
	err := pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&numRows)
	if err != nil {
		if err == pgx.ErrNoRows {
			// The requested database version isn't available to the given user
			return false, nil
		}
		log.Printf("Error when checking user's access to database '%s%s%s'. User: '%s' Error: %v\n",
			dbOwner, dbFolder, dbName, loggedInUser, err.Error())
		return false, err
	}

	// A row was returned, so the requested database IS available to the given user
	return true, nil
}

// Check if a username already exists in our system.  Returns true if the username is already taken, false if not.
// If an error occurred, the true/false value should be ignored, and only the error return code used.
func CheckUserExists(userName string) (bool, error) {
	dbQuery := `
		SELECT count(user_id)
		FROM users
		WHERE user_name = $1`
	var userCount int
	err := pdb.QueryRow(dbQuery, userName).Scan(&userCount)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return true, err
	}
	if userCount == 0 {
		// Username isn't in system
		return false, nil
	}
	// Username IS in system
	return true, nil
}

// Returns the certificate for a given user.
func ClientCert(userName string) ([]byte, error) {
	var cert []byte
	err := pdb.QueryRow(`
		SELECT client_cert
		FROM users
		WHERE user_name = $1`, userName).Scan(&cert)
	if err != nil {
		log.Printf("Retrieving client cert for '%s' from database failed: %v\n", userName, err)
		return nil, err
	}

	return cert, nil
}

// Creates a connection pool to the PostgreSQL server.
func ConnectPostgreSQL() (err error) {
	pgPoolConfig := pgx.ConnPoolConfig{*pgConfig, PGConnections, nil, 2 * time.Second}
	pdb, err = pgx.NewConnPool(pgPoolConfig)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't connect to PostgreSQL server: %v\n", err))
	}

	// Log successful connection message
	log.Printf("Connected to PostgreSQL server: %v:%v\n", conf.Pg.Server, uint16(conf.Pg.Port))

	return nil
}

// Returns the ID number for a given user's database.
func databaseID(dbOwner string, dbFolder string, dbName string) (dbID int, err error) {
	// Retrieve the database id
	dbQuery := `
		SELECT db_id
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1)
			AND folder = $2
			AND db_name = $3`
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&dbID)
	if err != nil {
		log.Printf("Error looking up database id. Owner: '%s', Database: '%s'. Error: %v\n", dbOwner, dbName,
			err)
	}
	return
}

// Return a list of 1) users with public databases, 2) along with the logged in user's most recently modified database,
// including their private one(s).
func DB4SDefaultList(loggedInUser string) ([]UserInfo, error) {
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE user_name = $1
		), user_db_list AS (
			SELECT DISTINCT ON (db_id) db_id, last_modified
			FROM sqlite_databases
			WHERE user_id = u.user_id
		), most_recent_user_db AS (
			SELECT db_idm, last_modified
			FROM user_db_list
			ORDER BY last_modified DESC
			LIMIT 1
		), public_dbs AS (
			SELECT db_id, last_modified
			FROM sqlite_databases
			WHERE public = true
			ORDER BY last_modified DESC
		), public_users AS (
			SELECT DISTINCT ON (db.user_id) db.user_id, db.last_modified
			FROM public_dbs as pub, sqlite_databases AS db, most_recent_user_db AS usr
			WHERE db.db_id = pub.db_id OR db.db_id = usr.db_id
			ORDER BY db.user_id, db.last_modified DESC
		)
		SELECT user_name, pu.last_modified
		FROM public_users AS pu, users
		WHERE users.user_name = pu.user_id
		ORDER BY last_modified DESC`
	rows, err := pdb.Query(dbQuery, loggedInUser)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	var list []UserInfo
	for rows.Next() {
		var oneRow UserInfo
		err = rows.Scan(&oneRow.Username, &oneRow.LastModified)
		if err != nil {
			log.Printf("Error retrieving database list for user: %v\n", err)
			return nil, err
		}
		list = append(list, oneRow)
	}

	return list, nil
}

// Retrieve the details for a specific database
func DBDetails(DB *SQLiteDBinfo, loggedInUser string, dbOwner string, dbFolder string, dbName string, commitID string) error {
	// If no commit ID was supplied, we retrieve the latest commit one from the default branch
	var err error
	if commitID == "" {
		commitID, err = DefaultCommit(dbOwner, dbFolder, dbName)
		if err != nil {
			return err
		}
	}

	// Retrieve the database details
	dbQuery := `
		SELECT db.date_created, db.last_modified, db.watchers, db.stars, db.discussions, db.merge_requests,
			db.commits, $4::text AS commit_id, db.commit_list->$4::text->'tree'->'entries'->0 AS db_entry,
			db.branches, db.releases, db.contributors, db.one_line_description, db.full_description,
			db.default_table, db.public, db.source_url
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND db.folder = $2
			AND db.db_name = $3`

	// If the request is for another users database, ensure we only look up public ones
	if loggedInUser != dbOwner {
		dbQuery += `
			AND db.public = true`
	}

	// Generate a predictable cache key for this functions' metadata.  Probably not sharable with other functions
	// cached metadata
	mdataCacheKey := MetadataCacheKey("meta", loggedInUser, dbOwner, dbFolder, dbName, commitID)

	// Use a cached version of the query response if it exists
	ok, err := GetCachedData(mdataCacheKey, &DB)
	if err != nil {
		log.Printf("Error retrieving data from cache: %v\n", err)
	}
	if ok {
		// Data was in cache, so we use that
		return nil
	}

	// Retrieve the requested database details
	var defTable, fullDesc, oneLineDesc, sourceURL pgx.NullString
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName, commitID).Scan(&DB.Info.DateCreated,
		&DB.Info.LastModified, &DB.Info.Watchers, &DB.Info.Stars, &DB.Info.Discussions, &DB.Info.MRs,
		&DB.Info.Commits, &DB.Info.CommitID,
		&DB.Info.DBEntry,
		&DB.Info.Branches, &DB.Info.Releases, &DB.Info.Contributors, &oneLineDesc, &fullDesc, &defTable,
		&DB.Info.Public, &sourceURL)

	if err != nil {
		log.Printf("Error when retrieving database details: %v\n", err.Error())
		return errors.New("The requested database doesn't exist")
	}
	if !oneLineDesc.Valid {
		DB.Info.OneLineDesc = "No description"
	} else {
		DB.Info.OneLineDesc = oneLineDesc.String
	}
	if !fullDesc.Valid {
		DB.Info.FullDesc = "No full description"
	} else {
		DB.Info.FullDesc = fullDesc.String
	}
	if !defTable.Valid {
		DB.Info.DefaultTable = ""
	} else {
		DB.Info.DefaultTable = defTable.String
	}
	if !sourceURL.Valid {
		DB.Info.SourceURL = ""
	} else {
		DB.Info.SourceURL = sourceURL.String
	}
	// Remove the " marks on the start and end of the commit id
	DB.Info.CommitID = strings.Trim(DB.Info.CommitID, "\"")

	// Fill out the fields we already have data for
	DB.Info.Database = dbName
	DB.Info.Folder = dbFolder

	// Retrieve latest fork count
	// TODO: This can probably be folded into the above SQL query as a sub-select, as a minor optimisation
	dbQuery = `
		SELECT forks
		FROM sqlite_databases
		WHERE db_id = (
			SELECT root_database
			FROM sqlite_databases
			WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1)
			AND folder = $2
			AND db_name = $3)`
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&DB.Info.Forks)
	if err != nil {
		log.Printf("Error retrieving fork count for '%s%s%s': %v\n", dbOwner, dbFolder, dbName, err)
		return err
	}

	// Cache the database details
	err = CacheData(mdataCacheKey, DB, 120)
	if err != nil {
		log.Printf("Error when caching page data: %v\n", err)
	}

	return nil
}

// Returns the star count for a given database.
func DBStars(dbOwner string, dbFolder string, dbName string) (starCount int, err error) {
	// Get the ID number of the database
	dbID, err := databaseID(dbOwner, dbFolder, dbName)
	if err != nil {
		return -1, err
	}

	// Retrieve the updated star count
	dbQuery := `
		SELECT stars
		FROM sqlite_databases
		WHERE db_id = $1`
	err = pdb.QueryRow(dbQuery, dbID).Scan(&starCount)
	if err != nil {
		log.Printf("Error looking up star count for database '%s/%s'. Error: %v\n", dbOwner, dbName, err)
		return -1, err
	}
	return starCount, nil
}

// Returns the list of all database versions available to the requesting user
func DBVersions(loggedInUser string, dbOwner string, dbFolder string, dbName string) ([]string, error) {
	dbQuery := `
		SELECT jsonb_object_keys(commit_list) AS commits
		FROM sqlite_databases
		WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $1)
				AND folder = $2
				AND db_name = $3`
	if loggedInUser != dbOwner {
		// The request is for another users database, so only return public versions
		dbQuery += `
				AND public is true`
	}
	dbQuery += `
		ORDER BY commits DESC`
	rows, err := pdb.Query(dbQuery, dbOwner, dbFolder, dbName)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	var l []string
	for rows.Next() {
		var i string
		err = rows.Scan(&i)
		if err != nil {
			log.Printf("Error retrieving commit list for '%s%s%s': %v\n", dbOwner, dbFolder, dbName,
				err)
			return nil, err
		}
		l = append(l, i)
	}

	// Safety checks
	numResults := len(l)
	if numResults == 0 {
		return nil, errors.New("Empty list returned instead of commit list.  This shouldn't happen")
	}

	return l, nil
}

// Retrieve the default commit ID for specific database
func DefaultCommit(dbOwner string, dbFolder string, dbName string) (string, error) {
	// If no commit ID was supplied, we retrieve the latest commit one from the default branch
	dbQuery := `
		SELECT branch_heads->default_branch->'commit' AS commit_id
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
					FROM users
					WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	var commitID string
	err := pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&commitID)
	if err != nil {
		log.Printf("Error when retrieving head commit ID of default branch: %v\n", err.Error())
		return "", errors.New("Internal error when looking up database details")
	}
	return commitID, nil
}

// Deletes the latest commit from a given branch.
func DeleteLatestBranchCommit(dbOwner string, dbFolder string, dbName string, branchName string) error {
	// Begin a transaction
	tx, err := pdb.Begin()
	if err != nil {
		return err
	}
	// Set up an automatic transaction roll back if the function exits without committing
	defer tx.Rollback()

	// Retrieve the branch list for the database, as we'll use it a few times in this function
	dbQuery := `
		SELECT branch_heads
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	var branchList map[string]BranchEntry
	err = tx.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&branchList)
	if err != nil {
		log.Printf("Retreving branch list failed for database '%s%s%s': %v\n", dbOwner, dbFolder, dbName, err)
		return err
	}

	// Grab the Commit ID of the branch head
	branch, ok := branchList[branchName]
	if !ok {
		// We weren't able to retrieve the branch information, so it's likely the branch doesn't exist any more, or
		// some other weirdness is happening
		log.Printf("Although no database error occurred, we couldn't retrieve a commit ID for branch '%s' of "+
			"database '%s%s%s'.", branchName, dbOwner, dbFolder, dbName)
		return errors.New("Database error when attempting to delete the commit")
	}
	commitID := branch.Commit

	// Retrieve the entire commit list for the database, as we'll use it a few times in this function
	dbQuery = `
		SELECT commit_list
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	var commitList map[string]CommitEntry
	err = tx.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&commitList)
	if err != nil {
		log.Printf("Retreving commit list failed for database '%s%s%s': %v\n", dbOwner, dbFolder, dbName, err)
		return err
	}

	// Ensure we're not being asked to delete the last commit of a branch (eg ensure it has a non empty Parent field)
	headCommit, ok := commitList[commitID]
	if !ok {
		log.Printf("Something went wrong retrieving commit '%s' from the commit list of database "+
			"'%s%s%s'\n", commitID, dbOwner, dbFolder, dbName)
		return errors.New("Error when retrieving commit information for the database")
	}
	if headCommit.Parent == "" {
		log.Printf("Error.  Not going to remove the last commit of branch '%s' on database '%s%s%s'\n",
			branchName, dbOwner, dbFolder, dbName)
		return errors.New("Removing the only remaining commit for a branch isn't allowed")
	}

	// Walk the other branches, checking if the commit is used in any of them.  If it is, we'll still move the branch
	// head back by one, but we'd better not remove the commit itself from the commit_list in the database
	foundElsewhere := false
	for bName, bEntry := range branchList {
		if bName == branchName {
			// No need to walk the tree for the branch we're deleting from
			continue
		}
		c := CommitEntry{Parent: bEntry.Commit}
		for c.Parent != "" {
			c, ok = commitList[c.Parent]
			if !ok {
				log.Printf("Error when walking the commit history of '%s%s%s', looking for commit '%s' in branch '%s'\n",
					dbOwner, dbFolder, dbName, c.Parent, bName)
				return errors.New("Error when attempting to remove the commit")
			}
			if c.ID == commitID {
				// The commit is being used by other branches, so we'd better not delete it from the commit_list in
				// the database
				foundElsewhere = true
				break
			}
		}
	}

	// Update the branch head to point at the previous commit
	branch.Commit = headCommit.Parent
	branchList[branchName] = branch
	dbQuery = `
		WITH our_db AS (
			SELECT db_id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $1
				)
				AND folder = $2
				AND db_name = $3
		)
		UPDATE sqlite_databases AS db
		SET branch_heads = $4
		FROM our_db
		WHERE db.db_id = our_db.db_id`
	commandTag, err := tx.Exec(dbQuery, dbOwner, dbFolder, dbName, branchList)
	if err != nil {
		log.Printf("Moving branch '%s' back one commit failed for database '%s%s%s': %v\n", branchName, dbOwner,
			dbFolder, dbName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf(
			"Wrong number of rows (%v) affected when moving branch '%s' back one commit for database '%s%s%s'\n",
			numRows, branchName, dbOwner, dbFolder, dbName)
	}

	// If needed remove the commit from the commit list
	delete(commitList, commitID)
	if !foundElsewhere {
		dbQuery = `
		WITH our_db AS (
			SELECT db_id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $1
				)
				AND folder = $2
				AND db_name = $3
		)
		UPDATE sqlite_databases AS db
		SET commit_list = $4
		FROM our_db
		WHERE db.db_id = our_db.db_id`
		commandTag, err := tx.Exec(dbQuery, dbOwner, dbFolder, dbName, commitList)
		if err != nil {
			log.Printf("Removing commit '%s' failed for database '%s%s%s': %v\n", commitID, dbOwner, dbFolder, dbName,
				err)
			return err
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf(
				"Wrong number of rows (%v) affected when removing commit '%s' for database '%s%s%s'\n", numRows,
				commitID, dbOwner, dbFolder, dbName)
		}

	}

	// Update the commit counter for the database
	dbQuery = `
		WITH the_db AS (
			SELECT db_id, jsonb_object_keys(commit_list)
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $1
				)
				AND folder = $2
				AND db_name = $3
		), commit_count AS (
			SELECT count(*) AS total
			FROM the_db
		)
		UPDATE sqlite_databases AS db
		SET commits = commit_count.total
		FROM the_db, commit_count
		WHERE db.db_id = the_db.db_id`
	commandTag, err = tx.Exec(dbQuery, dbOwner, dbFolder, dbName)
	if err != nil {
		log.Printf("Updating commit count failed for database '%s%s%s': %v\n", dbOwner, dbFolder, dbName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf(
			"Wrong number of rows (%v) affected when updating commit count for database '%s%s%s'\n", numRows,
			dbOwner, dbFolder, dbName)
	}

	// Commit the transaction
	err = tx.Commit()
	if err != nil {
		return err
	}
	return nil
}

// Disconnects the PostgreSQL database connection.
func DisconnectPostgreSQL() {
	pdb.Close()
}

// Fork the PostgreSQL entry for a SQLite database from one user to another
func ForkDatabase(srcOwner string, dbFolder string, dbName string, dstOwner string) (int, error) {
	// Copy the main database entry
	dbQuery := `
		WITH dst_u AS (
			SELECT user_id
			FROM users
			WHERE user_name = $1
		)
		INSERT INTO sqlite_databases (user_id, folder, db_name, public, forks, one_line_description, full_description,
			commits,  branches, contributors,
			root_database, default_table, source_url, commit_list, branch_heads, tags, default_branch,
			forked_from)
		SELECT dst_u.user_id, folder, db_name, public, forks, one_line_description, full_description,
			commits,  branches, contributors,
			root_database, default_table, source_url, commit_list, branch_heads, tags, default_branch,
			db_id
		FROM sqlite_databases, dst_u
		WHERE sqlite_databases.user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $2
			)
			AND folder = $3
			AND db_name = $4`
	commandTag, err := pdb.Exec(dbQuery, dstOwner, srcOwner, dbFolder, dbName)
	if err != nil {
		log.Printf("Forking database '%s%s%s' in PostgreSQL failed: %v\n", srcOwner, dbFolder, dbName, err)
		return 0, err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows affected (%d) when forking main database entry: "+
			"'%s%s%s' to '%s%s%s'\n", numRows, srcOwner, dbFolder, dbName, dstOwner, dbFolder, dbName)
	}

	// Increment the forks count for the root database
	dbQuery = `
		UPDATE sqlite_databases
		SET forks = forks + 1
		WHERE db_id = (
			SELECT root_database
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $1
				)
				AND folder = $2
				AND db_name = $3
			)
		RETURNING forks`
	var newForks int
	err = pdb.QueryRow(dbQuery, dstOwner, dbFolder, dbName).Scan(&newForks)
	if err != nil {
		log.Printf("Updating fork count in PostgreSQL failed: %v\n", err)
		return 0, err
	}
	return newForks, nil
}

// Checks if the given database was forked from another, and if so returns that one's owner, folder and database name
func ForkedFrom(dbOwner string, dbFolder string, dbName string) (forkOwn string, forkFol string, forkDB string,
	err error) {
	// Check if the database was forked from another
	var dbID, forkedFrom pgx.NullInt64
	dbQuery := `
		SELECT db_id, forked_from
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1)
			AND folder = $2
			AND db_name = $3`
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&dbID, &forkedFrom)
	if err != nil {
		log.Printf("Error checking if database was forked from another '%s%s%s'. Error: %v\n", dbOwner,
			dbFolder, dbName, err)
		return "", "", "", err
	}
	if !forkedFrom.Valid {
		// The database wasn't forked, so return empty strings
		return "", "", "", nil
	}

	// Return the details of the database this one was forked from
	dbQuery = `
		SELECT u.user_name, db.folder, db.db_name
		FROM users AS u, sqlite_databases AS db
		WHERE db.db_id = $1
			AND u.user_id = db.user_id`
	err = pdb.QueryRow(dbQuery, forkedFrom).Scan(&forkOwn, &forkFol, &forkDB)
	if err != nil {
		log.Printf("Error retrieving forked database information for '%s%s%s'. Error: %v\n", dbOwner,
			dbFolder, dbName, err)
		return "", "", "", err
	}
	return forkOwn, forkFol, forkDB, nil
}

// Return the complete fork tree for a given database
func ForkTree(loggedInUser string, dbOwner string, dbFolder string, dbName string) (outputList []ForkEntry, err error) {
	dbQuery := `
		SELECT users.user_name, db.folder, db.db_name, db.public, db.db_id, db.forked_from
		FROM sqlite_databases AS db, users
		WHERE db.root_database = (
				SELECT root_database
				FROM sqlite_databases
				WHERE user_id = (
						SELECT user_id
						FROM users
						WHERE user_name = $1
					)
					AND folder = $2
					AND db_name = $3
				)
			AND db.user_id = users.user_id
		ORDER BY db.forked_from NULLS FIRST`
	rows, err := pdb.Query(dbQuery, dbOwner, dbFolder, dbName)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	var dbList []ForkEntry
	for rows.Next() {
		var frk pgx.NullInt64
		var oneRow ForkEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.Folder, &oneRow.DBName, &oneRow.Public, &oneRow.ID, &frk)
		if err != nil {
			log.Printf("Error retrieving fork list for '%s%s%s': %v\n", dbOwner, dbFolder, dbName,
				err)
			return nil, err
		}
		if frk.Valid {
			oneRow.ForkedFrom = int(frk.Int64)
		}
		dbList = append(dbList, oneRow)
	}

	// Safety checks
	numResults := len(dbList)
	if numResults == 0 {
		return nil, errors.New("Empty list returned instead of fork tree.  This shouldn't happen")
	}
	if dbList[0].ForkedFrom != 0 {
		// The first entry has a non-zero forked_from field, indicating it's not the root entry.  That
		// shouldn't happen, so return an error.
		return nil, errors.New("Incorrect root entry data in retrieved database list")
	}

	// * Process the root entry *

	var iconDepth int
	var forkTrail []int

	// Set the root database ID
	rootID := dbList[0].ID

	// Set the icon list for display in the browser
	dbList[0].IconList = append(dbList[0].IconList, ROOT)

	// If the root database is no longer public, then use placeholder details instead
	if !dbList[0].Public {
		dbList[0].DBName = "private database"
	}

	// Append this completed database line to the output list
	outputList = append(outputList, dbList[0])

	// Append the root database ID to the fork trail
	forkTrail = append(forkTrail, rootID)

	// Mark the root database entry as processed
	dbList[0].Processed = true

	// Increment the icon depth
	iconDepth = 1

	// * Sort the remaining entries for correct display *
	numUnprocessedEntries := numResults - 1
	for numUnprocessedEntries > 0 {
		var forkFound bool
		outputList, forkTrail, forkFound = nextChild(loggedInUser, &dbList, &outputList, &forkTrail, iconDepth)
		if forkFound {
			numUnprocessedEntries--
			iconDepth++

			// Add stems and branches to the output icon list
			numOutput := len(outputList)

			myID := outputList[numOutput-1].ID
			myForkedFrom := outputList[numOutput-1].ForkedFrom

			// Scan through the earlier output list for any sibling entries
			var siblingFound bool
			for i := numOutput; i > 0 && siblingFound == false; i-- {
				thisID := outputList[i-1].ID
				thisForkedFrom := outputList[i-1].ForkedFrom

				if thisForkedFrom == myForkedFrom && thisID != myID {
					// Sibling entry found
					siblingFound = true
					sibling := outputList[i-1]

					// Change the last sibling icon to a branch icon
					sibling.IconList[iconDepth-1] = BRANCH

					// Change appropriate spaces to stems in the output icon list
					for l := numOutput - 1; l > i; l-- {
						thisEntry := outputList[l-1]
						if thisEntry.IconList[iconDepth-1] == SPACE {
							thisEntry.IconList[iconDepth-1] = STEM
						}
					}
				}
			}
		} else {
			// No child was found, so remove an entry from the fork trail then continue looping
			forkTrail = forkTrail[:len(forkTrail)-1]

			iconDepth--
		}
	}

	return outputList, nil
}

// Load the branch heads for a database.
// TODO: It might be better to have the default branch name be returned as part of this list, by indicating in the list
// TODO  which of the branches is the default.
func GetBranches(dbOwner string, dbFolder string, dbName string) (branches map[string]BranchEntry, err error) {
	dbQuery := `
		SELECT db.branch_heads
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND db.folder = $2
			AND db.db_name = $3`
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&branches)
	if err != nil {
		log.Printf("Error when retrieving branch heads for database '%s%s%s': %v\n", dbOwner, dbFolder, dbName,
			err)
		return nil, err
	}
	return branches, nil
}

// Retrieve the default branch name for a database.
func GetDefaultBranchName(dbOwner string, dbFolder string, dbName string) (string, error) {
	// Return the default branch name
	dbQuery := `
		SELECT db.default_branch
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND db.folder = $2
			AND db.db_name = $3`
	var branchName string
	err := pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&branchName)
	if err != nil {
		if err != pgx.ErrNoRows {
			log.Printf("Error when retrieving default branch name for database '%s%s%s': %v\n", dbOwner,
				dbFolder, dbName, err)
			return "", err
		} else {
			log.Printf("No default branch name exists for database '%s%s%s'. This shouldn't happen\n", dbOwner,
				dbFolder, dbName)
			return "", err
		}
	}
	return branchName, nil
}

// Retrieves the full commit list for a database.
func GetCommitList(dbOwner string, dbFolder string, dbName string) (map[string]CommitEntry, error) {
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE user_name = $1
		)
		SELECT commit_list as commits
		FROM sqlite_databases AS db, u
		WHERE db.user_id = u.user_id
			AND db.folder = $2
			AND db.db_name = $3`
	var l map[string]CommitEntry
	err := pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&l)
	if err != nil {
		log.Printf("Retrieving commit list for '%s%s%s' failed: %v\n", dbOwner, dbFolder, dbName, err)
		return map[string]CommitEntry{}, err
	}
	return l, nil
}

// Retrieve display name and email address for a given user.
func GetUserDetails(userName string) (string, string, error) {
	// Retrieve the values from the database
	dbQuery := `
		SELECT display_name, email
		FROM users
		WHERE user_name = $1`
	var dn, em pgx.NullString
	err := pdb.QueryRow(dbQuery, userName).Scan(&dn, &em)
	if err != nil {
		log.Printf("Error when retrieving display name and email for user '%s': %v\n", userName, err)
		return "", "", err
	}

	// Return the values which aren't NULL.  For those which are, return an empty string.
	var displayName, email string
	if dn.Valid {
		displayName = dn.String
	}
	if em.Valid {
		email = em.String
	}
	return displayName, email, err
}

// Returns the username associated with an email address.
func GetUsernameFromEmail(email string) (string, error) {
	dbQuery := `
		SELECT user_name
		FROM users
		WHERE email = $1`
	var u string
	err := pdb.QueryRow(dbQuery, email).Scan(&u)
	if err != nil {
		if err == pgx.ErrNoRows {
			// No matching username of the email
			return "", nil
		}
		log.Printf("Looking up username for email address '%s' failed: %v\n", email, err)
		return "", err
	}
	return u, nil
}

// Retrieve the highest version number of a database (if any), available to a given user.
// Use the empty string "" to retrieve the highest available public version.
func HighestDBVersion(dbOwner string, dbName string, dbFolder string, loggedInUser string) (ver int, err error) {
	dbQuery := `
		SELECT version
		FROM database_versions
		WHERE db = (
			SELECT idnum
			FROM sqlite_databases
			WHERE username = $1
				AND dbname = $2
				AND folder = $3`
	if dbOwner != loggedInUser {
		dbQuery += `
				AND public = true`
	}
	dbQuery += `
			)
		ORDER BY version DESC
		LIMIT 1`
	err = pdb.QueryRow(dbQuery, dbOwner, dbName, dbFolder).Scan(&ver)
	if err != nil && err != pgx.ErrNoRows {
		log.Printf("Error when retrieving highest database version # for '%s/%s'. Error: %v\n", dbOwner,
			dbName, err)
		return -1, err
	}
	if err == pgx.ErrNoRows {
		// No database versions seem to be present
		return 0, nil
	}
	return ver, nil
}

// Return the Minio bucket and ID for a given database. dbOwner, dbFolder, & dbName are from owner/folder/database URL
// fragment, // loggedInUser is the name for the currently logged in user, for access permission check.  Use an empty
// string ("") as the loggedInUser parameter if the true value isn't set or known.
// If the requested database doesn't exist, or the loggedInUser doesn't have access to it, then an error will be
// returned.
func MinioLocation(dbOwner string, dbFolder string, dbName string, commitID string, loggedInUser string) (string,
	string, error) {

	// TODO: This will likely need updating to query the "database_files" table to retrieve the Minio server name

	// If no commit was provided, we grab the default one
	if commitID == "" {
		var err error
		commitID, err = DefaultCommit(dbOwner, dbFolder, dbName)
		if err != nil {
			return "", "", err
		}
	}

	// Retrieve the sha256 for the requested commit's database file
	var dbQuery string
	dbQuery = `
		SELECT commit_list->$4::text->'tree'->'entries'->0->'sha256' AS sha256
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND db.folder = $2
			AND db.db_name = $3`

	// If the request is for another users database, it needs to be a public one
	if loggedInUser != dbOwner {
		dbQuery += `
				AND db.public = true`
	}

	var sha256 string
	err := pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName, commitID).Scan(&sha256)
	if err != nil {
		log.Printf("Error retrieving MinioID for %s/%s version %v: %v\n", dbOwner, dbName, commitID, err)
		return "", "", err
	}

	if sha256 == "" {
		// The requested database doesn't exist, or the logged in user doesn't have access to it
		return "", "", errors.New("The requested database wasn't found")
	}

	return sha256[:MinioFolderChars], sha256[MinioFolderChars:], nil
}

// Return the Minio bucket name for a given user.
func MinioUserBucket(userName string) (string, error) {
	var minioBucket string
	err := pdb.QueryRow(`
		SELECT minio_bucket
		FROM users
		WHERE username = $1`, userName).Scan(&minioBucket)
	if err != nil {
		if err == pgx.ErrNoRows {
			log.Printf("No known Minio bucket for user '%s'\n", userName)
			return "", errors.New("No known Minio bucket for that user")
		} else {
			log.Printf("Error when looking up Minio bucket name for user '%v': %v\n", userName, err)
			return "", err
		}
	}

	return minioBucket, nil
}

// Return the user's preference for maximum number of SQLite rows to display.
func PrefUserMaxRows(loggedInUser string) int {
	// Retrieve the user preference data
	dbQuery := `
		SELECT pref_max_rows
		FROM users
		WHERE user_id = (
			SELECT user_id
			FROM users
			WHERE user_name = $1)`
	var maxRows int
	err := pdb.QueryRow(dbQuery, loggedInUser).Scan(&maxRows)
	if err != nil {
		log.Printf("Error retrieving user '%s' preference data: %v\n", loggedInUser, err)
		return DefaultNumDisplayRows // Use the default value
	}

	return maxRows
}

// Return a list of users with public databases.
func PublicUserDBs() ([]UserInfo, error) {
	dbQuery := `
		WITH public_dbs AS (
			SELECT DISTINCT ON (user_id) user_id, last_modified
			FROM sqlite_databases
			WHERE public = true
			ORDER BY user_id, last_modified DESC
		)
		SELECT users.user_name, dbs.last_modified
		FROM public_dbs AS dbs, users
		WHERE users.user_id = dbs.user_id
		ORDER BY last_modified DESC`
	rows, err := pdb.Query(dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	var list []UserInfo
	for rows.Next() {
		var oneRow UserInfo
		err = rows.Scan(&oneRow.Username, &oneRow.LastModified)
		if err != nil {
			log.Printf("Error retrieving database list for user: %v\n", err)
			return nil, err
		}
		list = append(list, oneRow)
	}

	return list, nil
}

// Remove a database version from PostgreSQL.
func RemoveDBVersion(dbOwner string, folder string, dbName string, dbVersion int) error {
	dbQuery := `
		DELETE from database_versions
		WHERE db  = (	SELECT idnum
				FROM sqlite_databases
				WHERE username = $1
					AND folder = $2
					AND dbname = $3)
			AND version = $4`
	commandTag, err := pdb.Exec(dbQuery, dbOwner, folder, dbName, dbVersion)
	if err != nil {
		log.Printf("Removing database entry '%s' / '%s' / '%s' version %v failed: %v\n",
			dbOwner, folder, dbName, dbVersion, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows (%v) affected when removing database entry for '%s' / '%s' / '%s' version %v\n",
			numRows, dbOwner, folder, dbName, dbVersion)
		return nil
	}

	// Check if other versions of the database still exist
	dbQuery = `
		SELECT count(*) FROM database_versions
		WHERE db  = (	SELECT idnum
				FROM sqlite_databases
				WHERE username = $1
					AND folder = $2
					AND dbname = $3)`
	var numDBs int
	err = pdb.QueryRow(dbQuery, dbOwner, folder, dbName).Scan(&numDBs)
	if err != nil {
		// A real database error occurred
		log.Printf("Error checking if any further versions of database exist: %v\n", err)
		return err
	}

	// The database still has other versions, so there's nothing further to do
	if numDBs != 0 {
		return nil
	}

	// We removed the last version of the database, so now clean up the entry in the sqlite_databases table
	dbQuery = `
		DELETE FROM sqlite_databases
		WHERE username = $1
			AND folder = $2
			AND dbname = $3`
	commandTag, err = pdb.Exec(dbQuery, dbOwner, folder, dbName)
	if err != nil {
		log.Printf("Removing main entry for '%s' / '%s' / '%s' failed: %v\n", dbOwner, folder,
			dbName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows (%v) affected when removing main database entry for '%s' / '%s' / '%s'\n",
			numRows, dbOwner, folder, dbName)
	}

	return nil
}

// Rename a SQLite database.
func RenameDatabase(userName string, dbFolder string, dbName string, newName string) error {
	// Save the database settings
	SQLQuery := `
		UPDATE sqlite_databases
		SET dbname = $4
		WHERE username = $1
			AND folder = $2
			AND dbname = $3`
	commandTag, err := pdb.Exec(SQLQuery, userName, dbFolder, dbName, newName)
	if err != nil {
		log.Printf("Renaming database '%s%s%s' failed: %v\n", userName, dbFolder,
			dbName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		errMsg := fmt.Sprintf("Wrong number of rows affected (%v) when renaming '%s%s%s' to '%s%s%s'\n",
			numRows, userName, dbFolder, dbName, userName, dbFolder, newName)
		log.Printf(errMsg)
		return errors.New(errMsg)
	}

	// Log the rename
	log.Printf("Database renamed from '%s%s%s' to '%s%s%s'\n", userName, dbFolder, dbName, userName,
		dbFolder, newName)

	return nil
}

// Saves updated database settings to PostgreSQL.
func SaveDBSettings(userName string, dbFolder string, dbName string, oneLineDesc string, fullDesc string, defTable string, public bool, sourceURL string) error {
	// Check for values which should be NULL
	var nullable1LineDesc, nullableFullDesc, nullableSourceURL pgx.NullString
	if oneLineDesc == "" {
		nullable1LineDesc.Valid = false
	} else {
		nullable1LineDesc.String = oneLineDesc
		nullable1LineDesc.Valid = true
	}
	if fullDesc == "" {
		nullableFullDesc.Valid = false
	} else {
		nullableFullDesc.String = fullDesc
		nullableFullDesc.Valid = true
	}
	if sourceURL == "" {
		nullableSourceURL.Valid = false
	} else {
		nullableSourceURL.String = sourceURL
		nullableSourceURL.Valid = true
	}

	// Save the database settings
	SQLQuery := `
		UPDATE sqlite_databases
		SET one_line_description = $4, full_description = $5, default_table = $6, public = $7, source_url = $8
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	commandTag, err := pdb.Exec(SQLQuery, userName, dbFolder, dbName, nullable1LineDesc, nullableFullDesc, defTable,
		public, nullableSourceURL)
	if err != nil {
		log.Printf("Updating description for database '%s%s%s' failed: %v\n", userName, dbFolder,
			dbName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		errMsg := fmt.Sprintf("Wrong number of rows affected (%v) when updating description for '%s%s%s'\n",
			numRows, userName, dbFolder, dbName)
		log.Printf(errMsg)
		return errors.New(errMsg)
	}

	// Invalidate the old memcached entry for the database
	err = InvalidateCacheEntry(userName, userName, dbFolder, dbName, "") // Empty string indicates "for all versions"
	if err != nil {
		// Something went wrong when invalidating memcached entries for the database
		log.Printf("Error when invalidating memcache entries: %s\n", err.Error())
		return err
	}

	return nil
}

// Stores a certificate for a given client.
func SetClientCert(newCert []byte, userName string) error {
	SQLQuery := `
		UPDATE users
		SET client_certificate = $1
		WHERE username = $2`
	commandTag, err := pdb.Exec(SQLQuery, newCert, userName)
	if err != nil {
		log.Printf("Updating client certificate for '%s' failed: %v\n", userName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		errMsg := fmt.Sprintf("Wrong number of rows affected (%v) when storing client cert for '%s'\n",
			numRows, userName)
		log.Printf(errMsg)
		return errors.New(errMsg)
	}

	return nil
}

// Sets the user's preference for maximum number of SQLite rows to display.
func SetPrefUserMaxRows(userName string, maxRows int, displayName string, email string) error {
	dbQuery := `
		UPDATE users
		SET pref_max_rows = $2, display_name = $3, email = $4
		WHERE user_name = $1`
	commandTag, err := pdb.Exec(dbQuery, userName, maxRows, displayName, email)
	if err != nil {
		log.Printf("Updating user preferences failed for user '%s'. Error: '%v'\n", userName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows (%v) affected when updating user preferences. User: '%s'\n", numRows,
			userName)
	}
	return nil
}

// Set the email address for a user.
func SetUserEmail(userName string, email string) error {
	dbQuery := `
		UPDATE users
		SET email = $1
		WHERE user_name = $2`
	commandTag, err := pdb.Exec(dbQuery, email, userName)
	if err != nil {
		log.Printf("Updating user email failed: %v\n", err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when updating details for user '%v'\n", numRows, userName)
	}

	return nil
}

// Set the email address and password hash for a user.
func SetUserEmailPHash(userName string, email string, pHash []byte) error {
	dbQuery := `
		UPDATE users
		SET email = $1, password_hash = $2
		WHERE username = $3`
	commandTag, err := pdb.Exec(dbQuery, email, pHash, userName)
	if err != nil {
		log.Printf("Updating user email & password hash failed: %v\n", err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when updating details for user '%v'\n", numRows, userName)
	}

	return nil
}

// Retrieve the latest social stats for a given database.
func SocialStats(dbOwner string, dbFolder string, dbName string) (wa int, st int, fo int, err error) {

	// TODO: Implement caching of these stats

	// Retrieve latest star count
	dbQuery := `
		SELECT stars
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			AND folder = $2
			AND db_name = $3`
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&st)
	if err != nil {
		log.Printf("Error retrieving star count for '%s%s%s': %v\n", dbOwner, dbFolder, dbName, err)
		return -1, -1, -1, err
	}

	// Retrieve latest fork count
	dbQuery = `
		SELECT forks
		FROM sqlite_databases
		WHERE db_id = (
				SELECT root_database
				FROM sqlite_databases
				WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE user_name = $1
					)
			AND folder = $2
			AND db_name = $3)`
	err = pdb.QueryRow(dbQuery, dbOwner, dbFolder, dbName).Scan(&fo)
	if err != nil {
		log.Printf("Error retrieving fork count for '%s%s%s': %v\n", dbOwner, dbFolder, dbName, err)
		return -1, -1, -1, err
	}

	// TODO: Implement watchers
	return 0, st, fo, nil
}

// Updates the branches list for database.
func StoreBranches(dbOwner string, dbFolder string, dbName string, branches map[string]BranchEntry) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET branch_heads = $4, branches = $5
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
				)
			AND folder = $2
			AND db_name = $3`
	commandTag, err := pdb.Exec(dbQuery, dbOwner, dbFolder, dbName, branches, len(branches))
	if err != nil {
		log.Printf("Updating branch heads for database '%s%s%s' to '%v' failed: %v\n", dbOwner, dbFolder,
			dbName, branches, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf(
			"Wrong number of rows (%v) affected when updating branch heads for database '%s%s%s' to '%v'\n",
			numRows, dbOwner, dbFolder, dbName, branches)
	}
	return nil
}

// Stores database details in PostgreSQL, and the database data itself in Minio.
func StoreDatabase(dbOwner string, dbFolder string, dbName string, branches map[string]BranchEntry, c CommitEntry,
	pub bool, buf []byte, sha string, oneLineDesc string, fullDesc string, createDefBranch bool, branchName string,
	sourceURL string) error {
	// Store the database file
	err := StoreDatabaseFile(buf, sha)
	if err != nil {
		return err
	}

	// Check for values which should be NULL
	var nullable1LineDesc, nullableFullDesc pgx.NullString
	if oneLineDesc == "" {
		nullable1LineDesc.Valid = false
	} else {
		nullable1LineDesc.String = oneLineDesc
		nullable1LineDesc.Valid = true
	}
	if fullDesc == "" {
		nullableFullDesc.Valid = false
	} else {
		nullableFullDesc.String = fullDesc
		nullableFullDesc.Valid = true
	}

	// Store the database metadata
	cMap := map[string]CommitEntry{c.ID: c}
	var commandTag pgx.CommandTag
	dbQuery := `
		WITH root AS (
			SELECT nextval('sqlite_databases_db_id_seq') AS val
		)
		INSERT INTO sqlite_databases (user_id, db_id, folder, db_name, public, one_line_description, full_description,
			branch_heads, root_database, commit_list`
	if sourceURL != "" {
		dbQuery += `, source_url`
	}
	dbQuery +=
		`)
		SELECT (
			SELECT user_id
			FROM users
			WHERE user_name = $1), (SELECT val FROM root), $2, $3, $4, $5, $6, $8, (SELECT val FROM root), $7`
	if sourceURL != "" {
		dbQuery += `, $9`
	}
	dbQuery += `
		ON CONFLICT (user_id, folder, db_name)
			DO UPDATE
			SET commit_list = sqlite_databases.commit_list || $7,
				branch_heads = sqlite_databases.branch_heads || $8,
				last_modified = now(),
				commits = sqlite_databases.commits + 1`
	if sourceURL != "" {
		dbQuery += `,
			source_url = $9`
		commandTag, err = pdb.Exec(dbQuery, dbOwner, dbFolder, dbName, pub, nullable1LineDesc, nullableFullDesc,
			cMap, branches, sourceURL)
	} else {
		commandTag, err = pdb.Exec(dbQuery, dbOwner, dbFolder, dbName, pub, nullable1LineDesc, nullableFullDesc,
			cMap, branches)
	}
	if err != nil {
		log.Printf("Storing database '%s%s%s' failed: %v\n", dbOwner, dbFolder, dbName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%v) affected while storing database '%s%s%s'\n", numRows, dbOwner,
			dbFolder, dbName)
	}

	if createDefBranch {
		err = StoreDefaultBranchName(dbOwner, dbFolder, dbName, branchName)
		if err != nil {
			log.Printf("Storing default branch '%s' name for '%s%s%s' failed: %v\n", branchName, dbOwner,
				dbFolder, dbName, err)
			return err
		}
	}
	return nil
}

// Stores the default branch name for a database.
func StoreDefaultBranchName(dbOwner string, folder string, dbName string, branchName string) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET default_branch = $4
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
				)
			AND folder = $2
			AND db_name = $3`
	commandTag, err := pdb.Exec(dbQuery, dbOwner, folder, dbName, branchName)
	if err != nil {
		log.Printf("Changing default branch for database '%v' to '%v' failed: %v\n", dbName, branchName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%v) affected during update: database: %v, new branch name: '%v'\n",
			numRows, dbName, branchName)
	}
	return nil
}

// Toggle on or off the starring of a database by a user.
func ToggleDBStar(loggedInUser string, dbOwner string, dbFolder string, dbName string) error {
	// Check if the database is already starred
	starred, err := CheckDBStarred(loggedInUser, dbOwner, dbFolder, dbName)
	if err != nil {
		return err
	}

	// Get the ID number of the database
	dbID, err := databaseID(dbOwner, dbFolder, dbName)
	if err != nil {
		return err
	}

	// Add or remove the star
	if !starred {
		// Star the database
		insertQuery := `
			WITH u AS (
				SELECT user_id
				FROM users
				WHERE user_name = $2
			)
			INSERT INTO database_stars (db_id, user_id)
			SELECT $1, u.user_id
			FROM u`
		commandTag, err := pdb.Exec(insertQuery, dbID, loggedInUser)
		if err != nil {
			log.Printf("Adding star to database failed. Database ID: '%v' Username: '%s' Error '%v'\n",
				dbID, loggedInUser, err)
			return err
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf("Wrong # of rows affected (%v) when starring database ID: '%v' Username: '%s'\n",
				numRows, dbID, loggedInUser)
		}
	} else {
		// Unstar the database
		deleteQuery := `
		DELETE FROM database_stars
		WHERE db_id = $1
			AND user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $2
			)`
		commandTag, err := pdb.Exec(deleteQuery, dbID, loggedInUser)
		if err != nil {
			log.Printf("Removing star from database failed. Database ID: '%v' Username: '%s' Error: '%v'\n",
				dbID, loggedInUser, err)
			return err
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf("Wrong # of rows (%v) affected when unstarring database ID: '%v' Username: '%s'\n",
				numRows, dbID, loggedInUser)
		}
	}

	// Refresh the main database table with the updated star count
	updateQuery := `
		UPDATE sqlite_databases
		SET stars = (
			SELECT count(db_id)
			FROM database_stars
			WHERE db_id = $1
		) WHERE db_id = $1`
	commandTag, err := pdb.Exec(updateQuery, dbID)
	if err != nil {
		log.Printf("Updating star count in database failed: %v\n", err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when updating star count. Database ID: '%v'\n", numRows, dbID)
	}
	return nil
}

// Updates the contributors count for a database.
func UpdateContributorsCount(dbOwner string, dbFolder, dbName string) error {
	// Get the commit list for the database
	commitList, err := GetCommitList(dbOwner, dbFolder, dbName)
	if err != nil {
		return err
	}

	// Work out the new contributor count
	d := map[string]struct{}{}
	for _, k := range commitList {
		d[k.AuthorEmail] = struct{}{}
	}
	n := len(d)

	// Store the new contributor count in the database
	dbQuery := `
		UPDATE sqlite_databases
		SET contributors = $4
			WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
				AND folder = $2
				AND db_name = $3`
	commandTag, err := pdb.Exec(dbQuery, dbOwner, dbFolder, dbName, n)
	if err != nil {
		log.Printf("Updating contributor count in database '%s%s%s' failed: %v\n", dbOwner, dbFolder, dbName,
			err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when updating contributor count for database '%s%s%s'\n",
			numRows, dbOwner, dbFolder, dbName)
	}
	return nil
}

// Returns details for a user.
func User(userName string) (user UserDetails, err error) {
	dbQuery := `
		SELECT user_name, email, password_hash, date_joined, client_cert
		FROM users
		WHERE username = $1`
	err = pdb.QueryRow(dbQuery, userName).Scan(&user.Username, &user.Email, &user.PHash, &user.DateJoined,
		&user.ClientCert)
	if err != nil {
		if err == pgx.ErrNoRows {
			// The error was just "no such user found"
			return user, nil
		}

		// A real occurred
		log.Printf("Error retrieving details for user '%s' from database: %v\n", userName, err)
		return user, nil
	}

	return user, nil
}

// Returns the list of databases for a user.
func UserDBs(userName string, public AccessType) (list []DBInfo, err error) {
	// Construct SQL query for retrieving the requested database list
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE user_name = $1
		), default_commits AS (
			SELECT DISTINCT ON (db.db_name) db_name, db.db_id, db.branch_heads->db.default_branch->>'commit' AS id
			FROM sqlite_databases AS db, u
			WHERE db.user_id = u.user_id
		), dbs AS (
			SELECT DISTINCT ON (db.db_name) db.db_name, db.folder, db.date_created, db.last_modified, db.public,
				db.watchers, db.stars, db.discussions, db.merge_requests, db.commits, db.branches, db.releases,
				db.contributors, db.one_line_description, default_commits.id,
				db.commit_list->default_commits.id->'tree'->'entries'->0->'size' AS size, db.source_url
			FROM sqlite_databases AS db, default_commits
			WHERE db.db_id = default_commits.db_id`
	switch public {
	case DB_PUBLIC:
		// Only public databases
		dbQuery += ` AND db.public = true`
	case DB_PRIVATE:
		// Only private databases
		dbQuery += ` AND db.public = false`
	case DB_BOTH:
		// Both public and private, so no need to add a query clause
	default:
		// This clause shouldn't ever be reached
		return nil, fmt.Errorf("Incorrect 'public' value '%v' passed to UserDBs() function.", public)
	}
	dbQuery += `
		)
		SELECT *
		FROM dbs
		ORDER BY last_modified DESC`
	rows, err := pdb.Query(dbQuery, userName)
	if err != nil {
		log.Printf("Getting list of databases for user failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var desc, source pgx.NullString
		var oneRow DBInfo
		err = rows.Scan(&oneRow.Database, &oneRow.Folder, &oneRow.DateCreated, &oneRow.LastModified, &oneRow.Public,
			&oneRow.Watchers, &oneRow.Stars, &oneRow.Discussions, &oneRow.MRs, &oneRow.Commits, &oneRow.Branches,
			&oneRow.Releases, &oneRow.Contributors, &desc, &oneRow.CommitID, &oneRow.Size, &source)
		if err != nil {
			log.Printf("Error retrieving database list for user: %v\n", err)
			return nil, err
		}
		if !desc.Valid {
			oneRow.OneLineDesc = ""
		} else {
			oneRow.OneLineDesc = fmt.Sprintf(": %s", desc.String)
		}
		if !source.Valid {
			oneRow.SourceURL = ""
		} else {
			oneRow.SourceURL = source.String
		}
		list = append(list, oneRow)
	}

	// Get fork count for each of the databases
	for i, j := range list {
		// Retrieve latest fork count
		dbQuery = `
			WITH u AS (
				SELECT user_id
				FROM users
				WHERE user_name = $1
			)
			SELECT forks
			FROM sqlite_databases, u
			WHERE db_id = (
				SELECT root_database
				FROM sqlite_databases
				WHERE user_id = u.user_id
					AND folder = $2
					AND db_name = $3)`
		err = pdb.QueryRow(dbQuery, userName, j.Folder, j.Database).Scan(&list[i].Forks)
		if err != nil {
			log.Printf("Error retrieving fork count for '%s%s%s': %v\n", userName, j.Folder,
				j.Database, err)
			return nil, err
		}
	}

	return list, nil
}

// Remove the user from the database.  This automatically removes their entries from sqlite_databases too, due
// to the ON DELETE CASCADE referential integrity constraint.
func UserDelete(userName string) error {
	dbQuery := `
		DELETE FROM users
		WHERE username = $1`
	commandTag, err := pdb.Exec(dbQuery, userName)
	if err != nil {
		log.Printf("Deleting user '%s' from the database failed: %v\n", userName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when deleting user '%s'\n", numRows, userName)
		return err
	}

	return nil
}

// Returns a list of all DBHub.io users.
func UserList() ([]UserDetails, error) {
	dbQuery := `
		SELECT username, email, password_hash, date_joined
		FROM users
		ORDER BY username ASC`
	rows, err := pdb.Query(dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()

	// Assemble the row data into a list
	var userList []UserDetails
	for rows.Next() {
		var u UserDetails
		err = rows.Scan(&u.Username, &u.Email, &u.PHash, &u.DateJoined)
		if err != nil {
			log.Printf("Error retrieving user list from database: %v\n", err)
			return nil, err
		}
		userList = append(userList, u)
	}

	return userList, nil
}

// Returns the username for a given Auth0 ID.
func UserNameFromAuth0ID(auth0id string) (string, error) {
	// Query the database for a username matching the given Auth0 ID
	dbQuery := `
		SELECT user_name
		FROM users
		WHERE auth0_id = $1`
	var userName string
	err := pdb.QueryRow(dbQuery, auth0id).Scan(&userName)
	if err != nil {
		if err == pgx.ErrNoRows {
			// No matching user for the given Auth0 ID
			return "", nil
		}

		// A real occurred
		log.Printf("Error looking up username in database: %v\n", err)
		return "", nil
	}

	return userName, nil
}

// Returns the password hash for a user.
func UserPasswordHash(userName string) ([]byte, error) {
	row := pdb.QueryRow("SELECT password_hash FROM public.users WHERE username = $1", userName)
	var passHash []byte
	err := row.Scan(&passHash)
	if err != nil {
		log.Printf("Error looking up password hash for username '%s'. Error: %v\n", userName, err)
		return nil, err
	}
	return passHash, nil
}

// Returns the list of databases starred by a user.
func UserStarredDBs(userName string) (list []DBEntry, err error) {
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE user_name = $1
		),
		stars AS (
			SELECT st.db_id, st.user_id, st.date_starred
			FROM database_stars AS st, u
			WHERE st.user_id = u.user_id
		)
		SELECT users.user_name, dbs.db_name, stars.date_starred
		FROM users, stars, sqlite_databases AS dbs
		WHERE stars.user_id = users.user_id
			AND stars.db_id = dbs.db_id
		ORDER BY date_starred DESC`
	rows, err := pdb.Query(dbQuery, userName)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oneRow DBEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.DateEntry)
		if err != nil {
			log.Printf("Error retrieving stars list for user: %v\n", err)
			return nil, err
		}
		list = append(list, oneRow)
	}

	return list, nil
}

// Returns the list of users who starred a database.
func UsersStarredDB(dbOwner string, dbFolder string, dbName string) (list []DBEntry, err error) {
	dbQuery := `
		WITH star_users AS (
			SELECT user_id, date_starred
			FROM database_stars
			WHERE db_id = (
				SELECT db_id
				FROM sqlite_databases
				WHERE user_id = (
						SELECT user_id
						FROM users
						WHERE user_name = $1
					)
					AND folder = $2
					AND db_name = $3
				)
		)
		SELECT users.user_name, star_users.date_starred
		FROM users, star_users
		  WHERE users.user_id = star_users.user_id`
	rows, err := pdb.Query(dbQuery, dbOwner, dbFolder, dbName)
	if err != nil {
		log.Printf("Database query failed: %v\n", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oneRow DBEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.DateEntry)
		if err != nil {
			log.Printf("Error retrieving list of stars for %s/%s: %v\n", dbOwner, dbName, err)
			return nil, err
		}
		list = append(list, oneRow)
	}
	return list, nil
}
