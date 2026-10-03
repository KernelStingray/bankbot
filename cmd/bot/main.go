package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"asakabankbot/internal/broker"
	"asakabankbot/internal/config"
	"asakabankbot/internal/delivery"
	"asakabankbot/internal/repository"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	log.Println("Запуск банковского бота...")

	// 1. Загрузка конфигурации
	cfg := config.LoadConfig()

	// 2. Подключение к БД
	db := repository.ConnectDB(cfg.DBUrl)
	// Убираем defer db.Close() отсюда, закроем вручную при остановке
	log.Println("Проверка структуры базы данных...")
	err := repository.RunMigrations(db, "migrations")
	if err != nil {
		log.Fatalf("Бот остановлен из-за ошибки миграций: %v", err)
	}

	// 3. Инициализация бота
	bot := delivery.StartBot(cfg.BotToken)

	// 4. Инициализация репозиториев
	userRepo := repository.NewUserRepository(db)
	depRepo := repository.NewDepartmentRepository(db)
	sessionRepo := repository.NewSessionRepository(db)
	opRepo := repository.NewOperatorRepository(db)
	adminRepo := repository.NewAdminRepository(db)

	// 5. Подключение к RabbitMQ
	rabbitConn := broker.ConnectRabbitMQ(cfg.RabbitMQUrl)
	// Убираем defer rabbitConn.Close()

	// Оптимизация надежности: мониторинг соединения с RabbitMQ
	go func() {
		err := <-rabbitConn.NotifyClose(make(chan *amqp.Error))
		if err != nil {
			log.Fatalf("КРИТИЧЕСКАЯ ОШИБКА: Соединение с RabbitMQ потеряно: %v. Бот будет перезапущен.", err)
		}
	}()

	publisher := broker.NewPublisher(rabbitConn)
	consumer := broker.NewConsumer(rabbitConn, bot, sessionRepo, opRepo)

	// 6. Запуск Consumer'ов (Слушателей очередей)
	consumer.StartTicketConsumer(1)
	consumer.StartTicketConsumer(2)
	consumer.StartTicketConsumer(3)
	consumer.StartMessageConsumer()

	// 7. Инициализация обработчика Telegram
	botHandler := delivery.NewBotHandler(bot, userRepo, depRepo, sessionRepo, opRepo, adminRepo, publisher)

	// 8. Контекст для фоновых задач (чтобы уметь их останавливать)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Фоновая служба контроля таймаутов чатов ---
	go func() {
		log.Println("Фоновая служба контроля таймаутов успешно запущена")
		for {
			select {
			case <-ctx.Done(): // Сигнал на остановку фоновой службы
				log.Println("Остановка фоновой службы таймаутов...")
				return
			case <-time.After(1 * time.Minute):
				expiredSessions, err := sessionRepo.CloseExpiredSessions(10)
				if err != nil {
					log.Printf("ФОН ОШИБКА: сбой при поиске истекших сессий: %v", err)
					continue
				}

				if len(expiredSessions) == 0 {
					continue
				}

				for _, sess := range expiredSessions {
					log.Printf("ФОН: Сессия #%d автоматически закрыта по таймауту.", sess.SessionID)

					userRepo.UpdateBotState(sess.ClientTgID, "main_menu")
					clientMsg := tgbotapi.NewMessage(sess.ClientTgID, "⏳ Диалог автоматически закрыт из-за отсутствия активности в течение 10 минут.")
					clientMsg.ReplyMarkup = delivery.MainMenuKeyboard("ru")
					bot.Send(clientMsg)

					if sess.OperatorID64 != 0 {
						isAdmin := adminRepo.IsAdmin(sess.OperatorID64)
						opRepo.UpdateOperatorStatus(sess.OperatorID64, "online")
						opMsg := tgbotapi.NewMessage(sess.OperatorID64, fmt.Sprintf("⏳ Сессия #%d автоматически закрыта: клиент не отвечал более 10 минут. Вы возвращены на смену.", sess.SessionID))
						opMsg.ReplyMarkup = delivery.OperatorMenuKeyboard("online", isAdmin)
						bot.Send(opMsg)
					}
				}
			}
		}
	}()

	// 9. Запуск получения обновлений Telegram в отдельной горутине
	go func() {
		log.Println("Приложение успешно запущено и ожидает сообщений...")
		botHandler.HandleUpdates()
	}()

	// ==============================================================
	// 10. GRACEFUL SHUTDOWN (Перехват сигналов системы)
	// ==============================================================

	// Создаем канал для получения сигналов от ОС
	quit := make(chan os.Signal, 1)
	// Говорим ОС посылать в этот канал сигналы SIGINT (Ctrl+C) и SIGTERM (Остановка Docker)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Блокируем главный поток, пока не прилетит сигнал
	<-quit
	log.Println("\nПолучен сигнал остановки (SIGINT/SIGTERM). Начинаем Graceful Shutdown...")

	// Останавливаем получение новых обновлений от Telegram
	bot.StopReceivingUpdates()
	log.Println("Остановлен прием новых сообщений Telegram.")

	// Останавливаем фоновую горутину таймаутов
	cancel()

	// Даем процессам пару секунд на завершение текущих операций
	time.Sleep(2 * time.Second)

	// Аккуратно закрываем соединения
	if err := rabbitConn.Close(); err != nil {
		log.Printf("Ошибка при закрытии RabbitMQ: %v", err)
	} else {
		log.Println("Соединение с RabbitMQ успешно закрыто.")
	}

	if err := db.Close(); err != nil {
		log.Printf("Ошибка при закрытии PostgreSQL: %v", err)
	} else {
		log.Println("Соединение с PostgreSQL успешно закрыто.")
	}

	log.Println("Бот корректно завершил работу. До свидания!")
}
