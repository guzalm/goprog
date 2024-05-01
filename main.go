package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strconv"

	//"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

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
	otp      string
}

// News structure represents a news article
type News struct {
	Title       string
	Description string
	Source      string
	URL         string
}

var (
	db        *sql.DB
	log       *logrus.Logger
	limiter   = rate.NewLimiter(1, 3) // Rate limit of 1 request per second with a burst of 3 requests
	templates = template.Must(template.ParseGlob("templates/*.html"))
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
	connStr := "user=postgres password=rayana2015 dbname=postgres sslmode=disable"
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Error opening database connection:", err)
		panic(err)
	}

	err = db.Ping()
	if err != nil {
		log.Fatal("Error connecting to the database:", err)
		panic(err)
	}

	log.Info("Connected to the database")

	// Create the users and products table if it doesn't exist
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
		username TEXT PRIMARY KEY,
		email TEXT UNIQUE,
		password TEXT,
		role TEXT,
		otp TEXT
	); CREATE TABLE IF NOT EXISTS products (
		id SERIAL PRIMARY KEY,
		name TEXT,
		size TEXT,
		price INT
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
		log.Error("Error fetching products from the database:", err)
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Size, &p.Price); err != nil {
			log.Error("Error scanning product row:", err)
			continue
		}
		products = append(products, p)
	}

	if err := rows.Err(); err != nil {
		log.Error("Error iterating over product rows:", err)
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
	from := ""
	password := ""
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

// RegisterHandler handles user registration
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
	_, err := db.Exec("INSERT INTO users (username, email, password, role, otp) VALUES ($1, $2, $3, $4, $5)", username, email, password, role, otp)
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

// LoginHandler handles user login
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
	err := db.QueryRow("SELECT username, email, role FROM users WHERE username = $1 AND password = $2 AND otp = $3", username, password, otp).
		Scan(&user.Username, &user.Email, &user.Role)
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

	// Redirect based on user role
	if user.Role == "admin" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/profile-edit", http.StatusSeeOther)
	}
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

	// Rate limiting check
	if !limiter.Allow() {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Fetch products from the database
	products, err := fetchProductsFromDB(filter, sortBy, page, pageSize)
	if err != nil {
		log.Error("Error fetching products from the database:", err)
		http.Error(w, "Error fetching products from the database", http.StatusInternalServerError)
		return
	}

	// Fetch news from NewsAPI
	apiKey := "84b7be9be9f746c8a5a08894ea376461"
	keyword := "fashion" // Replace with appropriate keyword
	newsList, err := fetchNewsFromAPI(apiKey, keyword)
	if err != nil {
		log.Error("Error fetching news from API:", err)
		// Handle the error, e.g., ignore or display an error message
	}

	// Prepare data for the template
	tmpl := templates.Lookup("index.html")
	if tmpl == nil {
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
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
	}

	// Render the template with the data
	tmpl.Execute(w, data)
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
		log.Error("Error fetching user profile from the database:", err)
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
		_, err := db.Exec("UPDATE users SET email=$1 AND password=$2 WHERE username=$3", email, password, username)
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
		log.Error("Error fetching products from the database:", err)
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
	}{
		Filter:   filter,
		SortBy:   sortBy,
		Products: products,
		Page:     page,
		PrevPage: page - 1,
		NextPage: page + 1,
		PageSize: pageSize,
	}

	tmpl.Execute(w, data)
}

func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/delete/"):]
	productID, err := strconv.Atoi(id)
	if err != nil {
		log.Error("Invalid product ID:", err)
		http.Error(w, "Invalid product ID", http.StatusBadRequest)
		return
	}

	_, err = db.Exec("DELETE FROM products WHERE id = $1", productID)
	if err != nil {
		log.Error("Error deleting from database:", err)
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
func GenerateProducts() []Product {
	var products []Product
	for i := 0; i < 100; i++ {
		products = append(products, Product{
			Name:  "golang",
			Size:  "s",
			Price: 55.0,
		})
	}
	return products
}

func AddProductPostHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	// Парсинг формы для получения товаров
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form", http.StatusInternalServerError)
		return
	}

	// Извлечение товаров из формы
	var products []Product
	for i := 1; ; i++ {
		// Получение значений для каждого товара, используя уникальные имена полей
		name := r.FormValue(fmt.Sprintf("name%d", i))
		size := r.FormValue(fmt.Sprintf("size%d", i))
		// Проверка наличия имени и размера
		if name == "" && size == "" {
			break
		}
		price, err := strconv.ParseFloat(r.FormValue(fmt.Sprintf("price%d", i)), 64)
		if err != nil {
			http.Error(w, "Invalid price", http.StatusBadRequest)
			return
		}
		products = append(products, Product{Name: name, Size: size, Price: price})
	}

	start := time.Now()

	// Вставка каждого товара в базу данных без использования горутин
	for _, product := range products {
		_, err := db.Exec("INSERT INTO products (name, size, price) VALUES ($1, $2, $3)", product.Name, product.Size, product.Price)
		if err != nil {
			fmt.Println("Error inserting into database:", err)
			// В случае ошибки вы можете здесь добавить логгирование или обработку ошибки
			return
		}
		fmt.Printf("New product added: Name=%s, Size=%s, Price=%.2f\n", product.Name, product.Size, product.Price)
	}

	elapsed := time.Since(start)

	fmt.Printf("Time taken to insert %d products: %s\n", len(products), elapsed)

	// Перенаправление на страницу администратора после добавления товаров
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func AddProductsWithConcurrency(numGoroutines int) {
	startTime := time.Now()

	// Создание канала для синхронизации завершения горутин
	done := make(chan struct{})
	defer close(done)

	// Создание канала для передачи ошибок из горутин в основной поток
	errCh := make(chan error, numGoroutines)

	// Запуск горутин для каждого товара
	for i := 0; i < numGoroutines; i++ {
		// Логика записи товара в базу данных
		// Здесь вы можете использовать вашу текущую логику записи товара
		// Пример:
		_, err := db.Exec("INSERT INTO products (name, size, price) VALUES ($1, $2, $3)", "Sample Product", "M", 50.0)
		if err != nil {
			errCh <- err // Отправляем ошибку в канал ошибок
			continue
		}

		// Отправляем сигнал об успешном завершении горутины
		done <- struct{}{}
	}

	// Ожидание завершения всех горутин
	for i := 0; i < numGoroutines; i++ {
		select {
		case <-done:
			// Горутина успешно завершилась
		case err := <-errCh:
			// Произошла ошибка в горутине
			fmt.Printf("Error in goroutine: %v\n", err)
			return
		}
	}

	// Вывод времени затраченного на выполнение всех горутин
	fmt.Printf("Time taken for %d goroutines: %s\n", numGoroutines, time.Since(startTime))
}

func AddProducts(numGoroutines int) {
	startTime := time.Now()

	// Логика записи товара в базу данных без использования горутин
	for i := 0; i < numGoroutines; i++ {
		// Здесь вы можете использовать вашу текущую логику записи товара
		// Пример:
		_, err := db.Exec("INSERT INTO products (name, size, price) VALUES ($1, $2, $3)", "Sample Product", "M", 50.0)
		if err != nil {
			fmt.Printf("Error inserting into database: %v\n", err)
			return
		}
	}

	// Вывод времени затраченного на выполнение всех горутин
	fmt.Printf("Time taken for %d goroutines: %s\n", numGoroutines, time.Since(startTime))
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

func main() {
	startTime := time.Now()
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
	products := GenerateProducts()
	for _, product := range products {
		_, err := db.Exec("INSERT INTO products (name, size, price) VALUES ($1, $2, $3)", product.Name, product.Size, product.Price)
		if err != nil {
			fmt.Println("Error inserting into database:", err)

			return
		}
		fmt.Printf("New product added: Name=%s, Size=%s, Price=%.2f\n", product.Name, product.Size, product.Price)
	}

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Set up routes
	http.HandleFunc("/register", RegisterHandler)
	http.HandleFunc("/login", LoginHandler)
	http.HandleFunc("/register-post", RegisterPostHandler)
	http.HandleFunc("/login-post", LoginPostHandler)
	http.HandleFunc("/logout", LogoutHandler)
	http.HandleFunc("/", IndexHandler)
	http.Handle("/admin", AuthMiddleware(http.HandlerFunc(AdminHandler)))
	http.HandleFunc("/profile-edit", ProfileEditHandler)
	http.HandleFunc("/profile-edit-post", ProfileEditPostHandler)
	http.HandleFunc("/delete/", DeleteHandler)
	http.HandleFunc("/add-product", AddProductHandler)
	http.HandleFunc("/add-product-post", AddProductPostHandler)
	http.HandleFunc("/edit/", EditProductHandler)
	http.HandleFunc("/edit-product-post/", EditProductPostHandler)

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

	// Вызываем функции для добавления товаров с разным количеством горутин
	AddProducts(0)

	// Run server in a goroutine for graceful shutdown
	go func() {
		log.Println("Server is running at http://127.0.0.1:8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("Server error:", err)
		}
	}()

	elapsedTime := time.Since(startTime)
	fmt.Printf("Time taken to add products without goroutines: %s\n", elapsedTime)
}
