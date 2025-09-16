package main

import (
	"context"
	"fmt"
	stdlog "log"
	"math/rand"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	errorCounter metric.Int64Counter
	latencyHist  metric.Float64Histogram
	cartGauge    metric.Int64UpDownCounter
	cartItems    int64
	otelLogger   log.Logger
	tracer       trace.Tracer
)

func initProvider(ctx context.Context) (func(context.Context) error, error) {
	// OTLP HTTP Exporters (instead of gRPC)
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithEndpoint("localhost:4318"),
	)
	if err != nil {
		return nil, err
	}
	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithInsecure(),
		otlpmetrichttp.WithEndpoint("localhost:4318"),
	)
	if err != nil {
		return nil, err
	}
	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithInsecure(),
		otlploghttp.WithEndpoint("localhost:4318"),
	)
	if err != nil {
		return nil, err
	}

	// Resource
	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(
			semconv.ServiceName("skundu-signoz-go-service"),
		),
	)
	if err != nil {
		return nil, err
	}

	// Tracer
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Meter
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	meter := mp.Meter("skundu-signoz-go-metrics")

	// Metrics
	errorCounter, _ = meter.Int64Counter("skundu_signoz_go_error_requests", metric.WithDescription("The number of error requests"))
	latencyHist, _ = meter.Float64Histogram("skundu_signoz_go_request_latency_ms", metric.WithDescription("The latency of requests"))
	cartGauge, _ = meter.Int64UpDownCounter("skundu_signoz_go_cart_items", metric.WithDescription("The number of items in the cart"))

	// Logger
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)
	otelLogger = global.GetLoggerProvider().Logger("skundu-signoz-go-logger")

	// Tracer instance
	tracer = tp.Tracer("skundu-signoz-go-tracer")

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return err
		}
		if err := mp.Shutdown(ctx); err != nil {
			return err
		}
		if err := lp.Shutdown(ctx); err != nil {
			return err
		}
		return nil
	}, nil
}

func convertAttributes(attrs []attribute.KeyValue) []log.KeyValue {
	logAttrs := make([]log.KeyValue, len(attrs))
	for i, attr := range attrs {
		// Convert attribute to log.KeyValue based on the value type
		switch attr.Value.Type() {
		case attribute.BOOL:
			logAttrs[i] = log.Bool(string(attr.Key), attr.Value.AsBool())
		case attribute.INT64:
			logAttrs[i] = log.Int64(string(attr.Key), attr.Value.AsInt64())
		case attribute.FLOAT64:
			logAttrs[i] = log.Float64(string(attr.Key), attr.Value.AsFloat64())
		case attribute.STRING:
			logAttrs[i] = log.String(string(attr.Key), attr.Value.AsString())
		default:
			// Fallback to string representation
			logAttrs[i] = log.String(string(attr.Key), attr.Value.AsString())
		}
	}
	return logAttrs
}

func logInfo(ctx context.Context, message string, attrs ...attribute.KeyValue) {
	record := log.Record{}
	record.SetSeverity(log.SeverityInfo)
	record.SetBody(log.StringValue(message))
	if len(attrs) > 0 {
		record.AddAttributes(convertAttributes(attrs)...)
	}
	otelLogger.Emit(ctx, record)
}

func logError(ctx context.Context, message string, attrs ...attribute.KeyValue) {
	record := log.Record{}
	record.SetSeverity(log.SeverityError)
	record.SetBody(log.StringValue(message))
	if len(attrs) > 0 {
		record.AddAttributes(convertAttributes(attrs)...)
	}
	otelLogger.Emit(ctx, record)
}

func main() {
	ctx := context.Background()
	
	// Initialize with a timeout to avoid hanging if SigNoz is not available
	initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	
	shutdown, err := initProvider(initCtx)
	if err != nil {
		stdlog.Printf("Warning: failed to init OpenTelemetry provider: %v", err)
		stdlog.Println("Starting without OpenTelemetry...")
		
		// Fallback: create a no-op logger and tracer
		otelLogger = global.GetLoggerProvider().Logger("skundu-signoz-go-logger")
		tracer = otel.GetTracerProvider().Tracer("skundu-signoz-go-tracer")
	} else {
		defer shutdown(ctx)
	}

	// Handlers
	http.HandleFunc("/cart/add", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "cart-add-endpoint")
		defer span.End()

		cartItems++
		if cartGauge != nil {
			cartGauge.Add(ctx, 1)
		}
		logInfo(ctx, "Item added to cart", attribute.Int64("cart_items", cartItems))
		fmt.Fprintf(w, "Item added. Cart size: %d\n", cartItems)
	})

	http.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := tracer.Start(r.Context(), "slow-endpoint")
		defer span.End()

		delay := time.Duration(rand.Intn(2000)) * time.Millisecond
		time.Sleep(delay)
		logInfo(ctx, "Slow endpoint hit", attribute.Int("delay_ms", int(delay.Milliseconds())))
		fmt.Fprintf(w, "Slept for %v\n", delay)

		if latencyHist != nil {
			latencyHist.Record(ctx, float64(time.Since(start).Milliseconds()))
		}
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})

	// 404 handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cart/add" && r.URL.Path != "/slow" && r.URL.Path != "/health" {
			ctx, span := tracer.Start(r.Context(), "error-endpoint")
			defer span.End()

			if errorCounter != nil {
				errorCounter.Add(ctx, 1)
			}
			logError(ctx, "Unknown endpoint hit", attribute.String("path", r.URL.Path))
			http.NotFound(w, r)
			return
		}
	})

	stdlog.Println("Starting server at :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		stdlog.Fatal(err)
	}
}