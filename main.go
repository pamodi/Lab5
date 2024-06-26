package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"

	"golang.org/x/crypto/bcrypt"

	"github.com/juju/ratelimit"
	_ "github.com/lib/pq"
)

const (
	host     = "localhost"
	port     = 5432
	user     = ""
	password = ""
	dbname   = ""
)

// JWT Key
var jwtKey = []byte("")

// Struct to represent claims
type Claims struct {
	Email string `json:"email"`
	jwt.StandardClaims
}

// Struct to represent token
type Token struct {
	Token    string
	ExpireAt time.Time
}

// Struct to represent invitation code
type InvitationCode struct {
	Code string `json:"code"`
	Used bool   `json:"used"`
}

// Struct to represent user
type User struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// Struct to represent invitation details
type Invitation struct {
	ID    int
	Email string
}

var (
	// Define a map to store IP addresses and their corresponding rate limiters
	ipLimiterMap = make(map[string]*ratelimit.Bucket)
	// Mutex to synchronize access to the map
	ipLimiterMapMutex sync.Mutex
)

func main() {
	// Setup database
	db := SetupDatabase()
	defer db.Close()

	// Define API endpoints
	http.HandleFunc("/token", GenerateTokenHandler())
	http.HandleFunc("/invite", AuthMiddleware(GenerateInvitationCodeHandler(db)))
	http.HandleFunc("/register", AuthMiddleware(RegisterHandler(db)))
	http.HandleFunc("/login", RateLimitMiddleware(AuthMiddleware((LoginHandler(db)))))

	fmt.Println("Server started on :8012")
	// Start HTTP server
	log.Fatal(http.ListenAndServe(":8012", nil))

	ProcessResendCodes(db)
}

// Setup database
func SetupDatabase() *sql.DB {
	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)

	db, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		log.Fatal(err)
	}

	err = db.Ping()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Successfully connected to the database")
	return db
}

// Generate token handler
func GenerateTokenHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get user email from request header
		value := r.PostFormValue("Email")
		if value == "" {
			http.Error(w, "Email header not found", http.StatusUnauthorized)
			return
		}

		// Generate JWT token
		tokenString, err := GenerateJWTToken(value)
		if err != nil {
			http.Error(w, "Failed to generate token", http.StatusInternalServerError)
			return
		}

		// Respond with JWT token
		response := Token{Token: tokenString, ExpireAt: time.Now().Add(2 * time.Hour)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// Generate JWT token
func GenerateJWTToken(username string) (string, error) {
	// Create JWT token claims
	claims := jwt.MapClaims{
		"email":  username,
		"expiry": time.Now().Add(2 * time.Hour).Unix(), // Token expires in 2 minutes
	}

	// Create JWT token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// Sign token with secret key
	tokenString, err := token.SignedString(jwtKey)
	if err != nil {
		fmt.Println(err)
		return "", err
	}

	return tokenString, nil
}

// Authentication middleware
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		email := r.FormValue("email")
		claims := jwt.MapClaims{
			"email":  email,
			"expiry": time.Now().Add(2 * time.Hour).Unix(),
		}

		// Extract JWT token from Authorization header
		tokenString := ExtractTokenFromHeader(r)
		if tokenString == "" {
			http.Error(w, "Authorization header not found", http.StatusUnauthorized)
			return
		}

		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			return jwtKey, nil
		})

		if err != nil || !token.Valid {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Check if "expiry" claim exists and is valid
		expClaim, ok := claims["expiry"].(float64)
		if !ok || expClaim == 0 {
			http.Error(w, "Expired token", http.StatusUnauthorized)
			return
		}

		// Proceed to the next handler
		next.ServeHTTP(w, r)
	}
}

// Function to extract token from header
func ExtractTokenFromHeader(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	tokenParts := strings.Split(authHeader, " ")
	if len(tokenParts) != 2 || strings.ToLower(tokenParts[0]) != "bearer" {
		return ""
	}
	return tokenParts[1]
}

// Generate invitation code
func GenerateInvitationCodeHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email := r.FormValue("email")

		// Generate random invitation code
		codeBytes := make([]byte, 16)
		_, err := rand.Read(codeBytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		code := base64.URLEncoding.EncodeToString(codeBytes)
		fmt.Println(code)
		// Insert the code into the database
		_, err = db.Exec("INSERT INTO invitation_codes (code, email, used) VALUES ($1, $2, false)", code, email)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Return the generated code
		invitationCode := InvitationCode{Code: code}
		json.NewEncoder(w).Encode(invitationCode)
		w.WriteHeader(http.StatusCreated)
	}
}

// Register handler
func RegisterHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Only POST method is allowed!", http.StatusMethodNotAllowed)
			return
		}

		var user User
		json.NewDecoder(r.Body).Decode(&user)

		// Check if invitation code exists for the user
		var codeExists bool
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM invitation_codes WHERE code=$1 AND email=$2)", user.Code, user.Email).Scan(&codeExists)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !codeExists {
			http.Error(w, "Invalid invitation code", http.StatusBadRequest)
			return
		}

		// Check if invitation code exists unused for the user
		var isCodeUsed bool
		err_IsCodeUsed := db.QueryRow("SELECT EXISTS(SELECT 1 FROM invitation_codes WHERE code=$1 AND used=true AND email=$2)", user.Code, user.Email).Scan(&isCodeUsed)
		if err_IsCodeUsed != nil {
			http.Error(w, err_IsCodeUsed.Error(), http.StatusInternalServerError)
			return
		}
		if isCodeUsed {
			http.Error(w, "Used invitation code", http.StatusBadRequest)
			return
		}

		// Check if invitation code exists and not expired for the user
		var isValidCode bool
		err_isValidCode := db.QueryRow("SELECT EXISTS(SELECT 1 FROM invitation_codes WHERE code=$1 AND used=false AND email=$2 AND expires_at > NOW() - INTERVAL '2 minutes')", user.Code, user.Email).Scan(&isValidCode)
		if err_isValidCode != nil {
			http.Error(w, err_isValidCode.Error(), http.StatusInternalServerError)
			return
		}
		if !isValidCode {
			http.Error(w, "Expired invitation code", http.StatusBadRequest)
			return
		}

		// Hash the password
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Insert user into the database
		_, err = db.Exec("INSERT INTO users (email, password_hash) VALUES ($1, $2)", user.Email, hashedPassword)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Mark the invitation code as used
		_, err = db.Exec("UPDATE invitation_codes SET used=true WHERE code=$1", user.Code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(user)
		fmt.Println("User registered successfully")
	}
}

// Login handler
func LoginHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenString := ExtractTokenFromHeader(r)
		if tokenString == "" {
			http.Error(w, "Authorization header not found", http.StatusUnauthorized)
			return
		}

		if r.Method != "POST" {
			http.Error(w, "Only POST method is allowed!", http.StatusMethodNotAllowed)
			return
		}

		var user User
		json.NewDecoder(r.Body).Decode(&user)

		var passwordHsh string
		err := db.QueryRow("SELECT password_hash FROM users WHERE email = $1", user.Email).Scan(&passwordHsh)
		if err != nil {
			http.Error(w, "User not found", http.StatusUnauthorized)
			return
		}

		// Compare passwords
		err = bcrypt.CompareHashAndPassword([]byte(passwordHsh), []byte(user.Password))
		if err != nil {
			http.Error(w, "Invalid credential!", http.StatusMethodNotAllowed)
			return
		}

		var user_id int64
		_ = db.QueryRow("SELECT id FROM users WHERE email = $1", user.Email).Scan(&user_id)

		// Insert user session into the database
		_, err = db.Exec("INSERT INTO sessions (user_id, token) VALUES ($1, $2)", user_id, tokenString)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Logged in successfully!\n")
	}
}

// Function to query the database for expired invitation codes
func GetExpiredInvitations(db *sql.DB) ([]Invitation, error) {
	rows, err := db.Query("SELECT id, email FROM invitation_codes WHERE used = false AND expires_at < NOW() - INTERVAL '2 minutes'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var expiredInvitations []Invitation
	for rows.Next() {
		var invitation Invitation
		if err := rows.Scan(&invitation.ID, &invitation.Email); err != nil {
			return nil, err
		}
		expiredInvitations = append(expiredInvitations, invitation)
	}

	return expiredInvitations, nil
}

// Schedule a background task to run periodically
func ProcessResendCodes(db *sql.DB) {

	for {
		// Query database for expired invitation codes
		expiredInvitations, err := GetExpiredInvitations(db)
		if err != nil {
			fmt.Println("Error querying expired invitations:", err)
			continue
		}

		// Resend invitation codes or send reminders to users
		for _, invitation := range expiredInvitations {
			err := ResendInvitation(invitation)
			if err != nil {
				fmt.Println("Error resending invitation:", err)
			}
		}

		// Wait for some time before running the background task again
		time.Sleep(2 * time.Hour)
	}
}

// Function to resend invitation or send reminder to user
func ResendInvitation(invitation Invitation) error {
	// TODO: Send new invitation code to the user's email
	fmt.Printf("Resending invitation to %s\n", invitation.Email)
	return nil
}

// Rate limit middleware
func RateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get the IP address of the client
		ip := r.RemoteAddr

		ipLimiterMapMutex.Lock()
		limiter, exists := ipLimiterMap[ip]
		if !exists {
			// Create a new rate limiter for the IP address
			limiter = ratelimit.NewBucketWithRate(1, 1)
			ipLimiterMap[ip] = limiter
		}
		ipLimiterMapMutex.Unlock()

		// Take a token from the rate limiter
		if limiter.TakeAvailable(1) == 0 {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	}
}
