package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
//	"github.com/jung-kurt/gofpdf"
	_ "github.com/lib/pq"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// Initialize DNS resolver to use Google's public DNS server
func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}
}

// Product structure represents a product in the store
type Product struct {
	ID    int
	Name  string
	Size  string
	Price float64
}

// User structure represents a user in the system
type User struct {
	Username string
	Email    string
	Password string
	Role     string
	ChatID   string
	OTP      string
}

// News structure represents a news article
type News struct {
	Title       string
	Description string
	Source      string
	URL         string
}

// Message structure represents a chat message
type Message struct {
	ID        int       `json:"id"`
	ChatID    int       `json:"chatId"`
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Transaction represents a transaction
type Transaction struct {
	ID            int
	ProductID     int
	ProductName   string
	ProductPrice  float64
	CustomerName  string
	CustomerEmail string
	Status        string
	Timestamp     time.Time
}

var (
	db        *sql.DB
	log       *logrus.Logger
	limiter   = rate.NewLimiter(1, 3) // Rate limit of 1 request per second with a burst of 3 requests
	templates = template.Must(template.ParseGlob("templates/*.html"))

	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	clients   = make(map[*websocket.Conn]bool)
	broadcast = make(chan Message)
	mu        sync.Mutex
)

func fetchNewsFromAPI(apiKey, keyword string) ([]News, error) {
	url := fmt.Sprintf("https://newsapi.org/v2/everything?q=%s&apiKey=%s&pageSize=5", keyword, apiKey)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response struct {
		Articles []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Source      struct {
				Name string `json:"name"`
			} `json:"source"`
			URL string `json:"url"`
		} `json:"articles"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}

	var newsList []News
	for _, article := range response.Articles {
		newsList = append(newsList, News{
			Title:       article.Title,
			Description: article.Description,
			Source:      article.Source.Name,
			URL:         article.URL,
		})
	}

	return newsList, nil
}

func initDB() *sql.DB {
	connStr := "user=postgres password=rayana2015 dbname=postgres host=127.0.0.1 sslmode=disable"
	db, err := sql.Open("postgres", connStr)
	if (err != nil) {
		log.Fatal("Error opening database connection:", err)
		panic(err)
	}

	err = db.Ping()
	if (err != nil) {
		log.Fatal("Error connecting to the database:", err)
		panic(err)
	}

	log.Info("Connected to the database")

	// Create the users, products, chats, messages, and transactions table if they don't exist
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
        username TEXT PRIMARY KEY,
        email TEXT UNIQUE,
        password TEXT,
        role TEXT,
        otp TEXT,
        chat_id TEXT UNIQUE
    ); 
    CREATE TABLE IF NOT EXISTS products (
        id SERIAL PRIMARY KEY,
        name TEXT,
        size TEXT,
        price FLOAT
    );
    CREATE TABLE IF NOT EXISTS chats (
        id SERIAL PRIMARY KEY,
        user_id TEXT,
        status TEXT DEFAULT 'able'
    );
    CREATE TABLE IF NOT EXISTS messages (
        id SERIAL PRIMARY KEY,
        chat_id INT,
        sender TEXT,
        content TEXT,
        timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );
    CREATE TABLE IF NOT EXISTS transactions (
        id SERIAL PRIMARY KEY,
        product_id INT,
        product_name TEXT,
        product_price FLOAT,
        customer_name TEXT,
        customer_email TEXT,
        status TEXT,
        timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`)
	if err != nil {
		log.Fatal(err)
	}

	return db
}

func fetchProductsFromDB(filter, sortBy string, page, pageSize int) ([]Product, error) {
	var products []Product

	var query string
	var args []interface{}

	if filter != "" {
		query = "SELECT id, name, size, price FROM products WHERE name ILIKE $1"
		args = append(args, "%"+filter+"%")
	} else {
		query = "SELECT id, name, size, price FROM products"
	}

	if sortBy != "" {
		if sortBy == "size" {
			query += " ORDER BY CASE size " +
				"WHEN 'xs' THEN 1 " +
				"WHEN 's' THEN 2 " +
				"WHEN 'm' THEN 3 " +
				"WHEN 'l' THEN 4 " +
				"WHEN 'xl' THEN 5 " +
				"WHEN 'xxl' THEN 6 " +
				"ELSE 7 " +
				"END"
		} else {
			query += " ORDER BY " + sortBy
		}
	}

	if filter != "" {
		query += " LIMIT $2 OFFSET $3"
		args = append(args, int64(pageSize), int64((page-1)*pageSize))
	} else {
		query += " LIMIT $1 OFFSET $2"
		args = append(args, int64(pageSize), int64((page-1)*pageSize))
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		log.Println("Error fetching products from the database:", err)
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Size, &p.Price); err != nil {
			log.Println("Error scanning product row:", err)
			continue
		}
		products = append(products, p)
	}

	if err := rows.Err(); err != nil {
		log.Println("Error iterating over product rows:", err)
		return nil, err
	}

	return products, nil
}

// AuthMiddleware is a middleware to check if the user is authenticated and has the admin role
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check authentication
		cookie, err := r.Cookie("username")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		username := cookie.Value

		// Fetch user from the database based on the username
		var user User
		err = db.QueryRow("SELECT username, email, role FROM users WHERE username = $1", username).Scan(&user.Username, &user.Email, &user.Role)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Check if the user has admin role
		if user.Role != "admin" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Call the next handler
		next.ServeHTTP(w, r)
	})
}

func sendEmail(to, subject, body string) error {
	from := "liazzatmazhitova@gmail.com"
	password := "pyga cxpi zjkq ljwy"
	smtpHost := "smtp.gmail.com"
	smtpPort := "587"

	// Compose the email message
	message := "From: " + from + "\n" +
		"To: " + to + "\n" +
		"Subject: " + subject + "\n\n" +
		body

	// Connect to the SMTP server
	auth := smtp.PlainAuth("", from, password, smtpHost)
	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, from, []string{to}, []byte(message))
	if err != nil {
		return err
	}

	return nil
}

// GenerateOTP generates a random OTP consisting of 6 digits
func GenerateOTP() string {
	randomNum, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		panic(err)
	}
	randomNum.Add(randomNum, big.NewInt(100000))
	return randomNum.String()
}

func GenerateChatID() string {
	randomNum, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		panic(err)
	}
	randomNum.Add(randomNum, big.NewInt(100000))
	return randomNum.String()
}

func IsLoggedIn(r *http.Request) bool {
	cookie, err := r.Cookie("username")
	if err == nil && cookie != nil && cookie.Value != "" {
		return true
	}
	return false
}

func RegisterHandler(w http.ResponseWriter, r *http.Request) {
	// Parse the HTML template file
	tmpl := templates.Lookup("register.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	// Execute the template
	tmpl.Execute(w, nil)
}

// RegisterPostHandler handles user registration
func RegisterPostHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	username := r.FormValue("username")
	email := r.FormValue("email")
	password := r.FormValue("password")
	role := ""
	otp := GenerateOTP()
	chatID := GenerateChatID()

	// Basic validation
	if username == "" || password == "" {
		http.Error(w, "Username and password are required", http.StatusBadRequest)
		return
	}

	if username == "assan" || username == "zhanerke" || username == "guzql" {
		role = "admin"
	} else {
		role = "user"
	}

	// Insert the new user into the database
	_, err := db.Exec("INSERT INTO users (username, email, password, role, otp, chat_id) VALUES ($1, $2, $3, $4, $5, $6)", username, email, password, role, otp, chatID)
	if err != nil {
		log.Println("Error registering user:", err)
		http.Error(w, "Registration failed", http.StatusInternalServerError)
		return
	}

	sendEmail(email, "Clothes Shop", "Welcome! You have been registered! Your OTP is "+otp)

	fmt.Fprintf(w, "User %s successfully registered", username)
}

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	// Parse the HTML template file
	tmpl := templates.Lookup("login.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	// Execute the template
	tmpl.Execute(w, nil)
}

// LoginPostHandler handles user login
func LoginPostHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	otp := r.FormValue("otp")

	// Basic validation
	if username == "" || password == "" {
		http.Error(w, "Username and password are required", http.StatusBadRequest)
		return
	}

	// Check if user exists in the database
	var user User
	err := db.QueryRow("SELECT username, email, role, chat_id FROM users WHERE username = $1 AND password = $2 AND otp = $3", username, password, otp).
		Scan(&user.Username, &user.Email, &user.Role, &user.ChatID)
	if err != nil {
		log.Println("Error logging in:", err)
		http.Error(w, "Login failed", http.StatusUnauthorized)
		return
	}

	otp = GenerateOTP()
	_, err = db.Exec("UPDATE users SET otp = $1 WHERE username = $2", otp, username)

	// Simulate session management by setting a cookie
	expiration := time.Now().Add(24 * time.Hour)
	cookie := http.Cookie{Name: "username", Value: username, Expires: expiration}
	http.SetCookie(w, &cookie)

	sendEmail(user.Email, "OTP Update", "You have been logged in! Your new OTP is "+otp)

	// Redirect to the main page after login
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	// Clear the username cookie to log out the user
	cookie := http.Cookie{
		Name:    "username",
		Value:   "",
		Expires: time.Now().Add(-time.Hour), // Set expiration in the past to delete the cookie
	}
	http.SetCookie(w, &cookie)

	// Redirect to the login page or any other page
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func IndexHandler(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	sortBy := r.URL.Query().Get("sort")

	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}

	pageSize, err := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if err != nil || pageSize < 1 {
		pageSize = 10
	}

	isLoggedIn := IsLoggedIn(r)

	var ableChatID int
	if isLoggedIn {
		cookie, err := r.Cookie("username")
		if err == nil {
			username := cookie.Value
			err := db.QueryRow("SELECT id FROM chats WHERE user_id = $1 AND status = 'able'", username).Scan(&ableChatID)
			if err != nil && err != sql.ErrNoRows {
				log.Println("Error checking for able chat:", err)
			}
		}
	}

	// Rate limiting check
	if !limiter.Allow() {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Fetch products from the database
	products, err := fetchProductsFromDB(filter, sortBy, page, pageSize)
	if err != nil {
		log.Println("Error fetching products from the database:", err)
		http.Error(w, "Error fetching products from the database", http.StatusInternalServerError)
		return
	}

	// Fetch news from NewsAPI
	apiKey := "84b7be9be9f746c8a5a08894ea376461"
	keyword := "fashion" // Replace with appropriate keyword
	newsList, err := fetchNewsFromAPI(apiKey, keyword)
	if err != nil {
		log.Println("Error fetching news from API:", err)
		// Handle the error, e.g., ignore or display an error message
	}

	// Prepare data for the template
	tmpl := templates.Lookup("index.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	chatID := "default_chat_id" // Use the appropriate chat ID
	messages, err := fetchMessages(0)
	if err != nil {
		log.Println("Error fetching messages:", err)
	}

	data := struct {
		Filter     string
		SortBy     string
		Products   []Product
		Page       int
		PrevPage   int
		NextPage   int
		PageSize   int
		IsLoggedIn bool
		News       []News
		Messages   []Message
		ChatID     string
		AbleChatID int
	}{
		Filter:     filter,
		SortBy:     sortBy,
		Products:   products,
		Page:       page,
		PrevPage:   page - 1,
		NextPage:   page + 1,
		PageSize:   pageSize,
		IsLoggedIn: isLoggedIn,
		News:       newsList,
		Messages:   messages,
		ChatID:     chatID,
		AbleChatID: ableChatID,
	}

	// Render the template with the data
	tmpl.Execute(w, data)
}
func BuyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		productID, err := strconv.Atoi(r.URL.Query().Get("productID"))
		if err != nil {
			http.Error(w, "Invalid product ID", http.StatusBadRequest)
			return
		}

		var product Product
		err = db.QueryRow("SELECT id, name, size, price FROM products WHERE id = $1", productID).
			Scan(&product.ID, &product.Name, &product.Size, &product.Price)
		if err != nil {
			http.Error(w, "Product not found", http.StatusNotFound)
			return
		}

		tmpl := templates.Lookup("buy.html")
		if tmpl == nil {
			http.Error(w, "Template not found", http.StatusInternalServerError)
			return
		}

		tmpl.Execute(w, product)
		return
	}

	if r.Method == http.MethodPost {
		productID, err := strconv.Atoi(r.FormValue("productID"))
		if err != nil {
			http.Error(w, "Invalid product ID", http.StatusBadRequest)
			return
		}

		customerName := r.FormValue("name")
		customerEmail := r.FormValue("email")
		cardNumber := r.FormValue("cardNumber")
		expirationDate := r.FormValue("expirationDate")
		cvv := r.FormValue("cvv")

		// Basic validation
		if customerName == "" || customerEmail == "" || cardNumber == "" || expirationDate == "" || cvv == "" {
			http.Error(w, "All fields are required", http.StatusBadRequest)
			return
		}

		// Fetch product details from the database
		var product Product
		err = db.QueryRow("SELECT id, name, price FROM products WHERE id = $1", productID).
			Scan(&product.ID, &product.Name, &product.Price)
		if err != nil {
			http.Error(w, "Product not found", http.StatusNotFound)
			return
		}

		// Create a new transaction
		var transactionID int
		err = db.QueryRow("INSERT INTO transactions (product_id, product_name, product_price, customer_name, customer_email, status) VALUES ($1, $2, $3, $4, $5, 'pending') RETURNING id",
			productID, product.Name, product.Price, customerName, customerEmail).Scan(&transactionID)
		if err != nil {
			log.Printf("Error creating transaction: %v", err)
			http.Error(w, "Error creating transaction", http.StatusInternalServerError)
			return
		}

		// Simulate payment processing (always assume success for this example)

		// Update transaction status to "paid"
		_, err = db.Exec("UPDATE transactions SET status = 'paid' WHERE id = $1", transactionID)
		if err != nil {
			log.Printf("Error updating transaction status: %v", err)
			http.Error(w, "Error updating transaction status", http.StatusInternalServerError)
			return
		}

		// Prepare email content
		subject := "Ваш чек на покупку"
		body := fmt.Sprintf(
			"Спасибо за покупку!\n\nИнформация о покупке:\nИмя покупателя: %s\nНазвание товара: %s\nЦена: $%.2f\nДата: %s\n",
			customerName, product.Name, product.Price, time.Now().Format("02-01-2006 15:04:05"),
		)

		// Send email
		err = sendEmail(customerEmail, subject, body)
		if err != nil {
			log.Printf("Error sending email: %v", err)
			http.Error(w, "Error sending email", http.StatusInternalServerError)
			return
		}

		// Redirect to a success page
		http.Redirect(w, r, "/success", http.StatusSeeOther)
	}
}

func sendEmailWithAttachment(to, subject, body string, attachment *bytes.Buffer, filename string) error {
	from := "liazzatmazhitova@gmail.com"
	password := "pyga cxpi zjkq ljwy"
	smtpHost := "smtp.gmail.com"
	smtpPort := "587"

	// Create the email message
	message := "From: " + from + "\n" +
		"To: " + to + "\n" +
		"Subject: " + subject + "\n\n" +
		body

	// Create a multipart message
	var msg bytes.Buffer
	msg.WriteString(message)
	msg.WriteString("\n\n")
	msg.Write(attachment.Bytes())

	// Connect to the SMTP server
	auth := smtp.PlainAuth("", from, password, smtpHost)
	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, from, []string{to}, msg.Bytes())
	if err != nil {
		return err
	}

	return nil
}

// ProfileEditHandler handles displaying the profile edit form
func ProfileEditHandler(w http.ResponseWriter, r *http.Request) {
	// Fetch user profile information from the database based on the logged-in user
	cookie, err := r.Cookie("username")
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	username := cookie.Value

	var user User
	err = db.QueryRow("SELECT username, email FROM users WHERE username = $1", username).Scan(&user.Username, &user.Email)
	if err != nil {
		log.Println("Error fetching user profile from the database:", err)
		http.Error(w, "Error fetching user profile from the database", http.StatusInternalServerError)
		return
	}

	// Parse the HTML template file
	tmpl := templates.Lookup("profile-edit.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	// Execute the template with user profile data
	tmpl.Execute(w, user)
}

// ProfileEditPostHandler handles updating the user's profile information
func ProfileEditPostHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	// Fetch user profile information from the form submission
	username := r.FormValue("username")
	email := r.FormValue("email")
	password := r.FormValue("password")

	// Update the user's profile in the database
	if password != "" {
		_, err := db.Exec("UPDATE users SET email=$1, password=$2 WHERE username=$3", email, password, username)
		if err != nil {
			log.Println("Error updating user profile in database:", err)
			http.Error(w, "Error updating user profile in database", http.StatusInternalServerError)
			return
		}
	} else {
		_, err := db.Exec("UPDATE users SET email=$1 WHERE username=$2", email, username)
		if err != nil {
			log.Println("Error updating user profile in database:", err)
			http.Error(w, "Error updating user profile in database", http.StatusInternalServerError)
			return
		}
	}

	// Redirect to the profile page or any other page after successful update
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func AdminHandler(w http.ResponseWriter, r *http.Request) {
	// Fetch active chats from the database
	rows, err := db.Query("SELECT id, user_id FROM chats WHERE status = 'able'")
	if err != nil {
		log.Println("Error fetching chats from the database:", err)
		http.Error(w, "Error fetching chats from the database", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var chats []struct {
		ID     int
		UserID string
	}
	for rows.Next() {
		var chat struct {
			ID     int
			UserID string
		}
		if err := rows.Scan(&chat.ID, &chat.UserID); err != nil {
			log.Println("Error scanning chat row:", err)
			continue
		}
		chats = append(chats, chat)
	}

	filter := r.URL.Query().Get("filter")
	sortBy := r.URL.Query().Get("sort")

	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}

	pageSize, err := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if err != nil || pageSize < 1 {
		pageSize = 10
	}

	// Rate limiting check
	if !limiter.Allow() {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	products, err := fetchProductsFromDB(filter, sortBy, page, pageSize)
	if err != nil {
		log.Println("Error fetching products from the database:", err)
		http.Error(w, "Error fetching products from the database", http.StatusInternalServerError)
		return
	}

	tmpl := templates.Lookup("admin.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	data := struct {
		Filter   string
		SortBy   string
		Products []Product
		Page     int
		PrevPage int
		NextPage int
		PageSize int
		Chats    []struct {
			ID     int
			UserID string
		}
	}{
		Filter:   filter,
		SortBy:   sortBy,
		Products: products,
		Page:     page,
		PrevPage: page - 1,
		NextPage: page + 1,
		PageSize: pageSize,
		Chats:    chats,
	}

	tmpl.Execute(w, data)
}

func fetchUsers() ([]User, error) {
	rows, err := db.Query("SELECT username, chat_id FROM users")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.Username, &user.ChatID); err != nil {
			return nil, err
		}
		users = append(users, user)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return users, nil
}

func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/delete/"):]
	productID, err := strconv.Atoi(id)
	if err != nil {
		log.Println("Invalid product ID:", err)
		http.Error(w, "Invalid product ID", http.StatusBadRequest)
		return
	}

	_, err = db.Exec("DELETE FROM products WHERE id = $1", productID)
	if err != nil {
		log.Println("Error deleting from database:", err)
		http.Error(w, "Error deleting from database", http.StatusInternalServerError)
		return
	}

	log.Printf("Product deleted with ID: %d\n", productID)

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func AddProductHandler(w http.ResponseWriter, r *http.Request) {
	tmpl := templates.Lookup("add-product.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	tmpl.Execute(w, nil)
}

func AddProductPostHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	_, err := db.Exec("INSERT INTO products (name, size, price) VALUES ($1, $2, $3)",
		r.FormValue("name"), r.FormValue("size"), r.FormValue("price"))
	if err != nil {
		fmt.Println("Error inserting into database:", err)
		http.Error(w, "Error inserting into database", http.StatusInternalServerError)
		return
	}

	fmt.Printf("New product added: Name=%s, Size=%s, Price=%s\n", r.FormValue("name"), r.FormValue("size"), r.FormValue("price"))

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func EditProductHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/edit/"):]
	productID, err := strconv.Atoi(id)
	if err != nil {
		http.Error(w, "Invalid product ID", http.StatusBadRequest)
		return
	}

	var product Product
	err = db.QueryRow("SELECT id, name, size, price FROM products WHERE id = $1", productID).
		Scan(&product.ID, &product.Name, &product.Size, &product.Price)
	if err != nil {
		fmt.Println("Error fetching product details:", err)
		http.Error(w, "Error fetching product details", http.StatusInternalServerError)
		return
	}

	tmpl := templates.Lookup("edit-product.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	tmpl.Execute(w, product)
}

func EditProductPostHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/edit-product-post/"):]
	productID, err := strconv.Atoi(id)
	if err != nil {
		http.Error(w, "Invalid product ID", http.StatusBadRequest)
		return
	}

	_, err = db.Exec("UPDATE products SET name=$1, size=$2, price=$3 WHERE id=$4",
		r.FormValue("name"), r.FormValue("size"), r.FormValue("price"), productID)
	if err != nil {
		fmt.Println("Error updating product in database:", err)
		http.Error(w, "Error updating product in database", http.StatusInternalServerError)
		return
	}

	fmt.Printf("Product updated with ID: %d\n", productID)

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Fatalf("Error upgrading to websocket: %v", err)
	}
	defer ws.Close()

	mu.Lock()
	clients[ws] = true
	mu.Unlock()

	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message: %v", err)
			mu.Lock()
			delete(clients, ws)
			mu.Unlock()
			break
		}

		// Insert message into the database
		_, err = db.Exec("INSERT INTO messages (chat_id, sender, content) VALUES ($1, $2, $3)",
			msg.ChatID, msg.Sender, msg.Content)
		if err != nil {
			log.Printf("Error inserting message into database: %v", err)
		}

		// Broadcast the message to all clients
		broadcast <- msg
	}
}

func fetchMessages(chatID int) ([]Message, error) {
	var messages []Message

	rows, err := db.Query("SELECT id, chat_id, sender, content, timestamp FROM messages WHERE chat_id = $1 ORDER BY timestamp ASC", chatID)
	if err != nil {
		log.Println("Error fetching messages from the database:", err)
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.Sender, &msg.Content, &msg.Timestamp); err != nil {
			log.Println("Error scanning message row:", err)
			continue
		}
		messages = append(messages, msg)
	}

	if err := rows.Err(); err != nil {
		log.Println("Error iterating over message rows:", err)
		return nil, err
	}

	return messages, nil
}

// handleMessages broadcasts messages to all clients
func handleMessages() {
	for {
		msg := <-broadcast
		mu.Lock()
		for client := range clients {
			err := client.WriteJSON(msg)
			if err != nil {
				log.Printf("Error writing message: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
		mu.Unlock()
	}
}

func CreateChatHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
        return
    }

    cookie, err := r.Cookie("username")
    if err != nil {
        http.Redirect(w, r, "/login", http.StatusSeeOther)
        return
    }

    username := cookie.Value

    var chatID int
    err = db.QueryRow("SELECT id FROM chats WHERE user_id = $1 AND status = 'able'", username).Scan(&chatID)
    if err == nil {
        // Able chat exists, redirect to it
        http.Redirect(w, r, fmt.Sprintf("/chat?chatID=%d&role=user", chatID), http.StatusSeeOther)
        return
    }

    if err != sql.ErrNoRows {
        log.Println("Error checking for existing able chat:", err)
        http.Error(w, "Error checking for existing chat", http.StatusInternalServerError)
        return
    }

    // No able chat exists, create a new one
    err = db.QueryRow("INSERT INTO chats (user_id) VALUES ($1) RETURNING id", username).Scan(&chatID)
    if err != nil {
        log.Println("Error creating chat:", err)
        http.Error(w, "Error creating chat", http.StatusInternalServerError)
        return
    }

    // Redirect to the new chat
    http.Redirect(w, r, fmt.Sprintf("/chat?chatID=%d&role=user", chatID), http.StatusSeeOther)
}

func CloseChatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	chatID, err := strconv.Atoi(r.URL.Path[len("/close-chat/"):])
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	_, err = db.Exec("UPDATE chats SET status = 'disable' WHERE id = $1", chatID)
	if err != nil {
		log.Println("Error closing chat:", err)
		http.Error(w, "Error closing chat", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func ChatHandler(w http.ResponseWriter, r *http.Request) {
	chatIDStr := r.URL.Query().Get("chatID")
	chatID, err := strconv.Atoi(chatIDStr)
	if err != nil {
		log.Println("Invalid chat ID:", err)
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}
	role := r.URL.Query().Get("role") // Получаем роль из URL-параметра
	messages, err := fetchMessages(chatID)
	if err != nil {
		log.Println("Error fetching messages:", err)
		http.Error(w, "Error fetching messages", http.StatusInternalServerError)
		return
	}

	tmpl := templates.Lookup("chat.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	data := struct {
		ChatID   int
		Role     string
		Messages []Message
	}{
		ChatID:   chatID,
		Role:     role,
		Messages: messages,
	}

	tmpl.Execute(w, data)
}

func main() {
	// Initialize logger
	log = logrus.New()
	log.SetFormatter(&logrus.JSONFormatter{})
	file, err := os.OpenFile("logfile.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(io.MultiWriter(file, os.Stdout))
	} else {
		log.Error("Failed to log to file, using default stderr")
	}

	// Initialize database
	db = initDB()
	defer db.Close()

	// Set up HTTP server
	server := &http.Server{
		Addr:    "127.0.0.1:8080",
		Handler: nil, // Your handler will be set later
	}

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Set up routes
	http.HandleFunc("/register", RegisterHandler)
	http.HandleFunc("/login", LoginHandler)
	http.HandleFunc("/register-post", RegisterPostHandler)
	http.HandleFunc("/login-post", LoginPostHandler)
	http.HandleFunc("/logout", LogoutHandler)
	http.HandleFunc("/", IndexHandler)
	http.HandleFunc("/buy", BuyHandler) // Handle buying products
	http.Handle("/admin", AuthMiddleware(http.HandlerFunc(AdminHandler)))
	http.HandleFunc("/profile-edit", ProfileEditHandler)
	http.HandleFunc("/profile-edit-post", ProfileEditPostHandler)
	http.HandleFunc("/delete/", DeleteHandler)
	http.HandleFunc("/add-product", AddProductHandler)
	http.HandleFunc("/add-product-post", AddProductPostHandler)
	http.HandleFunc("/edit/", EditProductHandler)
	http.HandleFunc("/edit-product-post/", EditProductPostHandler)

	// Chat routes
	http.HandleFunc("/create-chat", CreateChatHandler)
	http.HandleFunc("/close-chat/", CloseChatHandler)

	// WebSocket routes
	http.HandleFunc("/ws", handleConnections)
	go handleMessages()

	// Chat handler
	http.HandleFunc("/chat", ChatHandler)

	// Run server in a goroutine for graceful shutdown
	go func() {
		log.Println("Server is running at http://127.0.0.1:8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("Server error:", err)
		}
	}()

	// Handle graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Server is shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatal("Server shutdown error:", err)
	}

	log.Info("Server has stopped")
}
