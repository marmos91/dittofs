#!/bin/bash

# DittoFS Benchmark Suite
# This script runs comprehensive benchmarks including performance and memory profiling

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BENCHMARK_DIR="${PROJECT_ROOT}/benchmark_results"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULT_DIR="${BENCHMARK_DIR}/${TIMESTAMP}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Print banner
print_banner() {
    echo -e "${BLUE}"
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║              DittoFS Benchmark Suite                      ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo -e "${NC}"
}

# Print section header
print_section() {
    echo -e "\n${GREEN}▶ $1${NC}"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

# Print info
print_info() {
    echo -e "${BLUE}ℹ${NC} $1"
}

# Print warning
print_warning() {
    echo -e "${YELLOW}⚠${NC} $1"
}

# Print error
print_error() {
    echo -e "${RED}✗${NC} $1"
}

# Print success
print_success() {
    echo -e "${GREEN}✓${NC} $1"
}

# Create result directories
setup_directories() {
    print_section "Setting up directories"
    mkdir -p "${RESULT_DIR}"/{profiles,reports,raw}
    print_success "Created result directory: ${RESULT_DIR}"
}

# Check prerequisites
check_prerequisites() {
    print_section "Checking prerequisites"

    # Check if Go is installed
    if ! command -v go &> /dev/null; then
        print_error "Go is not installed"
        exit 1
    fi
    print_success "Go $(go version | awk '{print $3}') found"

    # Check if we can mount NFS
    if [[ "$OSTYPE" == "darwin"* ]]; then
        print_info "Running on macOS"
        if ! mount | grep -q nfs; then
            print_warning "No active NFS mounts detected. Ensure you have permission to mount."
        fi
    elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
        print_info "Running on Linux"
        if ! command -v mount.nfs &> /dev/null; then
            print_warning "nfs-common not installed. Some benchmarks may fail."
        fi
    fi

    # Check for graphviz (for pprof visualization)
    if ! command -v dot &> /dev/null; then
        print_warning "graphviz not installed. Profile visualizations will not be generated."
        print_info "Install with: brew install graphviz (macOS) or apt-get install graphviz (Linux)"
    fi
}

# Run standard benchmarks
run_benchmarks() {
    print_section "Running E2E Benchmarks"

    local bench_time=${BENCH_TIME:-10s}
    local bench_count=${BENCH_COUNT:-3}

    print_info "Benchmark time: ${bench_time}"
    print_info "Benchmark count: ${bench_count}"

    cd "${PROJECT_ROOT}"

    # Run benchmarks and save results
    print_info "Running benchmarks..."
    go test -bench=. \
        -benchtime="${bench_time}" \
        -count="${bench_count}" \
        -benchmem \
        -timeout=30m \
        ./test/e2e/... \
        2>&1 | tee "${RESULT_DIR}/raw/benchmark.txt"

    print_success "Benchmarks complete"
}

# Run CPU profiling
run_cpu_profile() {
    print_section "Running CPU Profiling"

    cd "${PROJECT_ROOT}"

    print_info "Profiling FileOperations..."
    go test -bench=BenchmarkE2E/memory/FileOperations \
        -cpuprofile="${RESULT_DIR}/profiles/cpu_file_memory.prof" \
        -benchtime=20s \
        ./test/e2e/... > /dev/null 2>&1 || true

    print_info "Profiling ReadThroughput..."
    go test -bench=BenchmarkE2E/memory/ReadThroughput \
        -cpuprofile="${RESULT_DIR}/profiles/cpu_read_memory.prof" \
        -benchtime=20s \
        ./test/e2e/... > /dev/null 2>&1 || true

    print_info "Profiling WriteThroughput..."
    go test -bench=BenchmarkE2E/memory/WriteThroughput \
        -cpuprofile="${RESULT_DIR}/profiles/cpu_write_memory.prof" \
        -benchtime=20s \
        ./test/e2e/... > /dev/null 2>&1 || true

    print_success "CPU profiling complete"
}

# Run memory profiling
run_memory_profile() {
    print_section "Running Memory Profiling"

    cd "${PROJECT_ROOT}"

    print_info "Profiling memory allocation patterns..."
    go test -bench=BenchmarkE2E/memory/WriteThroughput/100MB \
        -memprofile="${RESULT_DIR}/profiles/mem_write_100mb.prof" \
        -benchtime=10s \
        ./test/e2e/... > /dev/null 2>&1 || true

    go test -bench=BenchmarkE2E/memory/MixedWorkload \
        -memprofile="${RESULT_DIR}/profiles/mem_mixed.prof" \
        -benchtime=10s \
        ./test/e2e/... > /dev/null 2>&1 || true

    print_success "Memory profiling complete"
}

# Generate profile reports
generate_reports() {
    print_section "Generating Profile Reports"

    if ! command -v go &> /dev/null; then
        print_warning "Cannot generate reports: go tool pprof not available"
        return
    fi

    cd "${PROJECT_ROOT}"

    # Generate text reports for CPU profiles
    for profile in "${RESULT_DIR}"/profiles/cpu_*.prof; do
        if [ -f "$profile" ]; then
            local basename=$(basename "$profile" .prof)
            print_info "Generating report for ${basename}..."

            # Top functions by CPU time
            go tool pprof -text "$profile" > "${RESULT_DIR}/reports/${basename}_top.txt" 2>/dev/null || true

            # Generate SVG if graphviz is available
            if command -v dot &> /dev/null; then
                go tool pprof -svg "$profile" > "${RESULT_DIR}/reports/${basename}.svg" 2>/dev/null || true
            fi
        fi
    done

    # Generate text reports for memory profiles
    for profile in "${RESULT_DIR}"/profiles/mem_*.prof; do
        if [ -f "$profile" ]; then
            local basename=$(basename "$profile" .prof)
            print_info "Generating report for ${basename}..."

            # Memory allocation report
            go tool pprof -text -alloc_space "$profile" > "${RESULT_DIR}/reports/${basename}_alloc.txt" 2>/dev/null || true
            go tool pprof -text -inuse_space "$profile" > "${RESULT_DIR}/reports/${basename}_inuse.txt" 2>/dev/null || true

            # Generate SVG if graphviz is available
            if command -v dot &> /dev/null; then
                go tool pprof -svg -alloc_space "$profile" > "${RESULT_DIR}/reports/${basename}_alloc.svg" 2>/dev/null || true
            fi
        fi
    done

    print_success "Profile reports generated"
}

# Parse and summarize results
generate_summary() {
    print_section "Generating Summary Report"

    local summary_file="${RESULT_DIR}/SUMMARY.md"

    cat > "$summary_file" << EOF
# DittoFS Benchmark Results

**Date:** $(date)
**Platform:** $(uname -s) $(uname -m)
**Go Version:** $(go version)
**Commit:** $(git rev-parse --short HEAD 2>/dev/null || echo "unknown")

## Benchmark Configuration

- Benchmark Time: ${BENCH_TIME:-10s}
- Benchmark Count: ${BENCH_COUNT:-3}

## Results

### Store Type Comparison

EOF

    # Extract key metrics from benchmark output
    if [ -f "${RESULT_DIR}/raw/benchmark.txt" ]; then
        print_info "Parsing benchmark results..."

        # Add throughput comparison
        echo "### Throughput Comparison" >> "$summary_file"
        echo "" >> "$summary_file"
        echo "| Operation | Store Type | Throughput | Allocations |" >> "$summary_file"
        echo "|-----------|------------|------------|-------------|" >> "$summary_file"

        grep -E "Benchmark.*Throughput.*-" "${RESULT_DIR}/raw/benchmark.txt" | \
            awk '{
                split($1, parts, "/");
                store = parts[2];
                op = parts[3] "/" parts[4];
                gsub(/Benchmark[^\/]*\//, "", op);
                ns = $3;
                bytes = $5;
                allocs = $7;
                if (ns > 0 && bytes > 0) {
                    throughput = bytes / (ns / 1e9) / 1024 / 1024;
                    printf "| %s | %s | %.2f MB/s | %s |\n", op, store, throughput, allocs;
                }
            }' >> "$summary_file" || true

        echo "" >> "$summary_file"
        echo "### Operation Latency" >> "$summary_file"
        echo "" >> "$summary_file"
        echo "| Operation | Store Type | ns/op | B/op | allocs/op |" >> "$summary_file"
        echo "|-----------|------------|-------|------|-----------|" >> "$summary_file"

        grep -E "Benchmark.*FileOperations.*-" "${RESULT_DIR}/raw/benchmark.txt" | \
            awk '{
                split($1, parts, "/");
                if (length(parts) >= 4) {
                    store = parts[2];
                    op = parts[4];
                    printf "| %s | %s | %s | %s | %s |\n", op, store, $3, $5, $7;
                }
            }' >> "$summary_file" || true
    fi

    # Add profile information
    cat >> "$summary_file" << EOF

## Profile Data

CPU and memory profiles are available in:
- \`profiles/\` - Raw profile data (*.prof)
- \`reports/\` - Generated reports (*.txt, *.svg)

### Viewing Profiles Interactively

\`\`\`bash
# CPU profile
go tool pprof profiles/cpu_write_memory.prof

# Memory profile
go tool pprof -alloc_space profiles/mem_write_100mb.prof

# Web interface
go tool pprof -http=:8080 profiles/cpu_write_memory.prof
\`\`\`

## Raw Data

Complete benchmark output is available in \`raw/benchmark.txt\`

EOF

    print_success "Summary report generated: ${summary_file}"
}

# Compare with previous results
compare_results() {
    print_section "Comparing with Previous Results"

    # Find previous benchmark results
    local previous=$(find "${BENCHMARK_DIR}" -maxdepth 1 -type d -name "202*" | sort -r | sed -n '2p')

    if [ -z "$previous" ]; then
        print_warning "No previous benchmark results found for comparison"
        return
    fi

    print_info "Comparing with: $(basename "$previous")"

    if command -v benchstat &> /dev/null; then
        print_info "Running benchstat comparison..."
        benchstat "${previous}/raw/benchmark.txt" "${RESULT_DIR}/raw/benchmark.txt" > "${RESULT_DIR}/comparison.txt"
        print_success "Comparison saved to: ${RESULT_DIR}/comparison.txt"
    else
        print_warning "benchstat not installed. Install with: go install golang.org/x/perf/cmd/benchstat@latest"
    fi
}

# Main execution
main() {
    print_banner

    # Parse command line arguments
    RUN_BENCHMARKS=true
    RUN_CPU_PROFILE=false
    RUN_MEM_PROFILE=false
    COMPARE=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --cpu)
                RUN_CPU_PROFILE=true
                shift
                ;;
            --mem)
                RUN_MEM_PROFILE=true
                shift
                ;;
            --profile)
                RUN_CPU_PROFILE=true
                RUN_MEM_PROFILE=true
                shift
                ;;
            --compare)
                COMPARE=true
                shift
                ;;
            --no-bench)
                RUN_BENCHMARKS=false
                shift
                ;;
            --help)
                echo "Usage: $0 [OPTIONS]"
                echo ""
                echo "Options:"
                echo "  --cpu          Run CPU profiling"
                echo "  --mem          Run memory profiling"
                echo "  --profile      Run both CPU and memory profiling"
                echo "  --compare      Compare with previous results"
                echo "  --no-bench     Skip standard benchmarks"
                echo "  --help         Show this help message"
                echo ""
                echo "Environment Variables:"
                echo "  BENCH_TIME     Benchmark duration (default: 10s)"
                echo "  BENCH_COUNT    Number of benchmark runs (default: 3)"
                exit 0
                ;;
            *)
                print_error "Unknown option: $1"
                echo "Use --help for usage information"
                exit 1
                ;;
        esac
    done

    check_prerequisites
    setup_directories

    if [ "$RUN_BENCHMARKS" = true ]; then
        run_benchmarks
    fi

    if [ "$RUN_CPU_PROFILE" = true ]; then
        run_cpu_profile
    fi

    if [ "$RUN_MEM_PROFILE" = true ]; then
        run_memory_profile
    fi

    if [ "$RUN_CPU_PROFILE" = true ] || [ "$RUN_MEM_PROFILE" = true ]; then
        generate_reports
    fi

    if [ "$RUN_BENCHMARKS" = true ]; then
        generate_summary
    fi

    if [ "$COMPARE" = true ]; then
        compare_results
    fi

    print_section "Benchmark Complete"
    print_success "Results saved to: ${RESULT_DIR}"

    # Print quick stats
    if [ -f "${RESULT_DIR}/SUMMARY.md" ]; then
        echo ""
        print_info "Quick Summary:"
        echo ""
        cat "${RESULT_DIR}/SUMMARY.md" | head -20
    fi
}

# Run main function
main "$@"
