#!/bin/bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


# 文件上传下载客户端
# 支持 upload、download 子命令，带 MD5 校验

VERSION="1.0.0"
DEFAULT_SERVER="http://localhost:8080"
CONFIG_FILE="$HOME/.fileclient.conf"

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 加载配置文件
load_config() {
    if [ -f "$CONFIG_FILE" ]; then
        source "$CONFIG_FILE"
    else
        # 创建默认配置
        cat > "$CONFIG_FILE" << EOF
# 文件客户端配置文件
SERVER_URL="$DEFAULT_SERVER"
UPLOAD_ENDPOINT="/upload/md5"
DOWNLOAD_ENDPOINT="/download"
DELETE_ENDPOINT="/delete"
BATCH_UPLOAD_ENDPOINT="/upload/batch"
CHECK_MD5=true
VERBOSE=false
TIMEOUT=300
EOF
    fi
    
    # 设置默认值
    SERVER_URL=${SERVER_URL:-"$DEFAULT_SERVER"}
    UPLOAD_ENDPOINT=${UPLOAD_ENDPOINT:-"/upload/md5"}
    DOWNLOAD_ENDPOINT=${DOWNLOAD_ENDPOINT:-"/download"}
    DELETE_ENDPOINT=${DELETE_ENDPOINT:-"/delete"}
    BATCH_UPLOAD_ENDPOINT=${BATCH_UPLOAD_ENDPOINT:-"/upload/batch"}
    CHECK_MD5=${CHECK_MD5:-true}
    VERBOSE=${VERBOSE:-false}
    TIMEOUT=${TIMEOUT:-300}
}

# 保存配置
save_config() {
    cat > "$CONFIG_FILE" << EOF
# 文件客户端配置文件
SERVER_URL="$SERVER_URL"
UPLOAD_ENDPOINT="$UPLOAD_ENDPOINT"
DOWNLOAD_ENDPOINT="$DOWNLOAD_ENDPOINT"
DELETE_ENDPOINT="$DELETE_ENDPOINT"
BATCH_UPLOAD_ENDPOINT="$BATCH_UPLOAD_ENDPOINT"
CHECK_MD5="$CHECK_MD5"
VERBOSE="$VERBOSE"
TIMEOUT="$TIMEOUT"
EOF
}

# 计算文件的 MD5
calculate_md5() {
    local file="$1"
    
    if [ ! -f "$file" ]; then
        echo "错误: 文件不存在: $file" >&2
        return 1
    fi
    
    # 检测操作系统类型
    case "$(uname)" in
        Darwin)
            # macOS
            md5 -q "$file"
            ;;
        Linux)
            # Linux
            md5sum "$file" | awk '{print $1}'
            ;;
        CYGWIN*|MINGW*|MSYS*)
            # Windows Git Bash
            md5sum "$file" | awk '{print $1}'
            ;;
        *)
            # 其他系统，尝试各种方法
            if command -v md5sum >/dev/null 2>&1; then
                md5sum "$file" | awk '{print $1}'
            elif command -v md5 >/dev/null 2>&1; then
                md5 -q "$file"
            elif command -v openssl >/dev/null 2>&1; then
                openssl md5 "$file" | awk '{print $2}'
            else
                echo "错误: 未找到 MD5 计算工具" >&2
                return 1
            fi
            ;;
    esac
}

# 计算文件的 SHA256
calculate_sha256() {
    local file="$1"
    
    if [ ! -f "$file" ]; then
        echo "错误: 文件不存在: $file" >&2
        return 1
    fi
    
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl sha256 "$file" | awk '{print $2}'
    else
        echo "错误: 未找到 SHA256 计算工具" >&2
        return 1
    fi
}

# 显示进度条
show_progress() {
    local pid=$1
    local msg=$2
    local delay=0.1
    local spinstr='|/-\'
    
    printf "${BLUE}${msg}${NC} "
    
    while kill -0 $pid 2>/dev/null; do
        local temp=${spinstr#?}
        printf "\b${spinstr:0:1}"
        local spinstr=$temp${spinstr%"$temp"}
        sleep $delay
    done
    printf "\b "
    echo ""
}

# 上传单个文件
upload_file() {
    local file="$1"
    local server_url="$2"
    local check_md5="$3"
    
    if [ ! -f "$file" ]; then
        echo -e "${RED}错误: 文件不存在: $file${NC}" >&2
        return 1
    fi
    
    local filename=$(basename "$file")
    local md5_value=""
    
    # 如果需要计算 MD5
    if [ "$check_md5" = "true" ]; then
        echo -n "计算文件 MD5..."
        md5_value=$(calculate_md5 "$file")
        if [ $? -ne 0 ]; then
            echo -e "${RED}失败${NC}"
            return 1
        fi
        echo -e "${GREEN}完成${NC}"
        echo "文件 MD5: $md5_value"
    fi
    
    echo "正在上传文件: $filename"
    
    # 构建上传命令
    local curl_cmd="curl -s"
    
    if [ "$VERBOSE" = "true" ]; then
        curl_cmd="$curl_cmd -v"
    fi
    
    curl_cmd="$curl_cmd -X POST"
    
    if [ -n "$md5_value" ]; then
        curl_cmd="$curl_cmd -H 'X-File-MD5: $md5_value'"
    fi
    
    curl_cmd="$curl_cmd -F 'file=@$file'"
    curl_cmd="$curl_cmd '$server_url'"
    
    if [ "$VERBOSE" = "true" ]; then
        echo "执行命令: $curl_cmd"
    fi
    
    # 执行上传
    local response=$(eval $curl_cmd 2>&1)
    local exit_code=$?
    
    if [ $exit_code -ne 0 ]; then
        echo -e "${RED}上传失败 (curl 错误码: $exit_code)${NC}"
        if [ "$VERBOSE" = "true" ]; then
            echo "错误信息: $response"
        fi
        return 1
    fi
    
    # 解析 JSON 响应
    if command -v jq >/dev/null 2>&1; then
        # 使用 jq 解析 JSON
        local success=$(echo "$response" | jq -r '.success // false')
        local message=$(echo "$response" | jq -r '.message // ""')
        local file_md5=$(echo "$response" | jq -r '.file_md5 // ""')
        local md5_match=$(echo "$response" | jq -r '.md5_match // "false"')
        
        if [ "$success" = "true" ]; then
            echo -e "${GREEN}上传成功: $message${NC}"
            
            if [ -n "$file_md5" ]; then
                echo "服务器计算的 MD5: $file_md5"
                
                if [ "$md5_match" = "true" ]; then
                    echo -e "${GREEN}✓ MD5 校验通过${NC}"
                elif [ "$md5_match" = "false" ]; then
                    echo -e "${RED}✗ MD5 校验失败${NC}"
                fi
            fi
        else
            echo -e "${RED}上传失败: $message${NC}"
            return 1
        fi
    else
        # 如果没有 jq，直接显示原始响应
        echo "服务器响应: $response"
    fi
    
    return 0
}

# 批量上传文件
batch_upload() {
    local files=("$@")
    local server_url="$2"
    
    if [ ${#files[@]} -eq 0 ]; then
        echo -e "${RED}错误: 请指定要上传的文件${NC}" >&2
        return 1
    fi
    
    echo "准备上传 ${#files[@]} 个文件..."
    
    # 计算所有文件的 MD5
    local md5_list=()
    local file_args=""
    
    for file in "${files[@]}"; do
        if [ ! -f "$file" ]; then
            echo -e "${RED}警告: 文件不存在: $file，跳过${NC}" >&2
            continue
        fi
        
        local filename=$(basename "$file")
        echo -n "计算 $filename 的 MD5..."
        
        local md5_value=$(calculate_md5 "$file")
        if [ $? -eq 0 ]; then
            md5_list+=("$md5_value")
            file_args="$file_args -F 'files=@$file'"
            echo -e "${GREEN}完成${NC}"
        else
            echo -e "${RED}失败，跳过此文件${NC}"
        fi
    done
    
    if [ ${#md5_list[@]} -eq 0 ]; then
        echo -e "${RED}错误: 没有有效的文件可上传${NC}" >&2
        return 1
    fi
    
    # 构建 MD5 列表字符串
    local md5_header_value=$(IFS=,; echo "${md5_list[*]}")
    
    echo "开始批量上传..."
    
    # 构建 curl 命令
    local curl_cmd="curl -s"
    
    if [ "$VERBOSE" = "true" ]; then
        curl_cmd="$curl_cmd -v"
    fi
    
    curl_cmd="$curl_cmd -X POST"
    curl_cmd="$curl_cmd -H 'X-File-MD5-List: $md5_header_value'"
    curl_cmd="$curl_cmd $file_args"
    curl_cmd="$curl_cmd '$server_url'"
    
    if [ "$VERBOSE" = "true" ]; then
        echo "执行命令: $curl_cmd"
    fi
    
    # 执行上传
    local response=$(eval $curl_cmd 2>&1)
    local exit_code=$?
    
    if [ $exit_code -ne 0 ]; then
        echo -e "${RED}批量上传失败 (curl 错误码: $exit_code)${NC}"
        if [ "$VERBOSE" = "true" ]; then
            echo "错误信息: $response"
        fi
        return 1
    fi
    
    # 解析响应
    if command -v jq >/dev/null 2>&1; then
        echo "$response" | jq '.'
    else
        echo "服务器响应: $response"
    fi
    
    return 0
}

# 下载文件
download_file() {
    local filename="$1"
    local output="$2"
    local server_url="$3"
    
    if [ -z "$filename" ]; then
        echo -e "${RED}错误: 请指定要下载的文件名${NC}" >&2
        return 1
    fi
    
    if [ -z "$output" ]; then
        output="$filename"
    fi
    
    echo "正在下载文件: $filename"
    echo "保存到: $output"
    
    # 构建下载命令
    local curl_cmd="curl -s"
    
    if [ "$VERBOSE" = "true" ]; then
        curl_cmd="$curl_cmd -v"
    fi
    
    curl_cmd="$curl_cmd -o '$output'"
    curl_cmd="$curl_cmd '$server_url?filename=$(echo $filename | sed "s/ /%20/g")'"
    
    if [ "$VERBOSE" = "true" ]; then
        echo "执行命令: $curl_cmd"
    fi
    
    # 执行下载
    eval $curl_cmd &
    local curl_pid=$!
    
    show_progress $curl_pid "下载中"
    
    wait $curl_pid
    local exit_code=$?
    
    if [ $exit_code -ne 0 ]; then
        echo -e "${RED}下载失败 (curl 错误码: $exit_code)${NC}"
        
        # 删除可能损坏的文件
        if [ -f "$output" ]; then
            rm -f "$output"
        fi
        
        return 1
    fi
    
    if [ ! -f "$output" ]; then
        echo -e "${RED}错误: 下载失败，文件未创建${NC}"
        return 1
    fi
    
    # 检查文件大小
    local file_size=$(stat -f%z "$output" 2>/dev/null || stat -c%s "$output" 2>/dev/null || echo "0")
    
    if [ "$file_size" -eq 0 ]; then
        echo -e "${RED}警告: 下载的文件大小为 0${NC}"
        return 1
    fi
    
    echo -e "${GREEN}下载完成${NC}"
    echo "文件大小: $file_size 字节"
    
    # 尝试获取服务器返回的 MD5
    local server_md5=""
    if curl -s -I "$server_url?filename=$(echo $filename | sed "s/ /%20/g")" 2>/dev/null | grep -i "X-File-MD5" >/dev/null 2>&1; then
        server_md5=$(curl -s -I "$server_url?filename=$(echo $filename | sed "s/ /%20/g")" | grep -i "X-File-MD5" | cut -d' ' -f2 | tr -d '\r')
    fi
    
    if [ -n "$server_md5" ] && [ "$CHECK_MD5" = "true" ]; then
        echo "服务器提供的 MD5: $server_md5"
        echo -n "计算下载文件的 MD5..."
        
        local local_md5=$(calculate_md5 "$output")
        if [ $? -eq 0 ]; then
            echo -e "${GREEN}完成${NC}"
            echo "本地计算的 MD5: $local_md5"
            
            if [ "$local_md5" = "$server_md5" ]; then
                echo -e "${GREEN}✓ MD5 校验通过${NC}"
            else
                echo -e "${RED}✗ MD5 校验失败，文件可能已损坏${NC}"
                return 1
            fi
        else
            echo -e "${YELLOW}警告: 无法计算 MD5${NC}"
        fi
    fi
    
    return 0
}

delete_file() {
    local filename="$1"
    local server_url="$2"

    if [ -z "$filename" ]; then
        echo -e "${RED}错误: 请指定要删除的文件名${NC}" >&2
        return 1
    fi
    
    local md5_value=""
    
    # 如果需要计算 MD5
    if [ "$check_md5" = "true" ]; then
        echo -n "计算文件 MD5..."
        md5_value=$(calculate_md5 "$filename")
        if [ $? -ne 0 ]; then
            echo -e "${RED}失败${NC}"
            return 1
        fi
        echo -e "${GREEN}完成${NC}"
        echo "文件 MD5: $md5_value"
    fi
    
    echo "正在删除文件: $filename"

     # 构建上传命令
    local curl_cmd="curl -s"
    
    if [ "$VERBOSE" = "true" ]; then
        curl_cmd="$curl_cmd -v"
    fi
    
    curl_cmd="$curl_cmd -X POST"
    
    if [ -n "$md5_value" ]; then
        curl_cmd="$curl_cmd -H 'X-File-MD5: $md5_value'"
    fi
    
    curl_cmd="$curl_cmd '$server_url'?filename=$filename"
    
    if [ "$VERBOSE" = "true" ]; then
        echo "执行命令: $curl_cmd"
    fi
    
    # 执行上传
    local response=$(eval $curl_cmd 2>&1)
    local exit_code=$?
    
    if [ $exit_code -ne 0 ]; then
        echo -e "${RED}删除失败 (curl 错误码: $exit_code)${NC}"
        if [ "$VERBOSE" = "true" ]; then
            echo "错误信息: $response"
        fi
        return 1
    fi
    
    # 解析 JSON 响应
    if command -v jq >/dev/null 2>&1; then
        # 使用 jq 解析 JSON
        local success=$(echo "$response" | jq -r '.success // false')
        local message=$(echo "$response" | jq -r '.message // ""')
        local file_md5=$(echo "$response" | jq -r '.file_md5 // ""')
        local md5_match=$(echo "$response" | jq -r '.md5_match // "false"')
        
        if [ "$success" = "true" ]; then
            echo -e "${GREEN}删除成功: $message${NC}"
        else
            echo -e "${RED}删除失败: $message${NC}"
            return 1
        fi
    else
        # 如果没有 jq，直接显示原始响应
        echo "服务器响应: $response"
    fi
    
    return 0
}

# 列出服务器上的文件
list_files() {
    local server_url="$1"
    
    echo "正在获取文件列表..."
    
    # 尝试从 /api/files 端点获取文件列表
    local list_endpoint="$server_url/api/files"
    
    local curl_cmd="curl -s"
    if [ "$VERBOSE" = "true" ]; then
        curl_cmd="$curl_cmd -v"
    fi
    
    curl_cmd="$curl_cmd '$list_endpoint'"
    
    local response=$(eval $curl_cmd 2>&1)
    local exit_code=$?
    
    if [ $exit_code -ne 0 ]; then
        echo -e "${YELLOW}警告: 无法获取文件列表 (端点可能不存在)${NC}"
        return 1
    fi
    
    if command -v jq >/dev/null 2>&1; then
        echo "$response" | jq -r '.files[]?' 2>/dev/null || echo "$response"
    else
        echo "$response"
    fi
    
    return 0
}

# 显示帮助信息
show_help() {
    cat << EOF
文件上传下载客户端 v$VERSION

用法: $0 [命令] [选项] [参数]

命令:
  upload <文件1> [文件2...]   上传一个或多个文件
  download <文件名> [输出路径] 下载文件
  list                        列出服务器上的文件
  config                      显示或修改配置
  version                    显示版本信息
  help                       显示此帮助信息

选项:
  -s, --server <URL>         服务器地址 (默认: $DEFAULT_SERVER)
  -m, --no-md5               禁用 MD5 校验
  -o, --output <路径>        指定下载文件的输出路径
  -v, --verbose              显示详细输出
  -h, --help                 显示帮助信息

示例:
  $0 upload document.pdf
  $0 upload image1.jpg image2.png
  $0 download report.pdf
  $0 download report.pdf -o /tmp/report.pdf
  $0 upload data.txt -s http://192.168.1.100:8080
  $0 config set SERVER_URL http://example.com:8080
  $0 config show

配置位置: $CONFIG_FILE
EOF
}

# 显示版本信息
show_version() {
    echo "文件上传下载客户端 v$VERSION"
    echo "服务器地址: $SERVER_URL"
    echo "MD5 校验: $CHECK_MD5"
    echo "详细模式: $VERBOSE"
}

# 配置管理
manage_config() {
    local action="$1"
    local key="$2"
    local value="$3"
    
    case "$action" in
        show)
            echo -e "${BLUE}当前配置:${NC}"
            echo "配置文件: $CONFIG_FILE"
            echo ""
            cat "$CONFIG_FILE" | while read line; do
                if [[ ! "$line" =~ ^# ]] && [[ -n "$line" ]]; then
                    echo "  $line"
                fi
            done
            ;;
        set)
            if [ -z "$key" ] || [ -z "$value" ]; then
                echo -e "${RED}用法: $0 config set <键> <值>${NC}" >&2
                echo -e "${YELLOW}可用配置项: SERVER_URL, UPLOAD_ENDPOINT, DOWNLOAD_ENDPOINT, DELETE_ENDPOINT, CHECK_MD5, VERBOSE, TIMEOUT${NC}"
                return 1
            fi
            
            # 验证键名
            case "$key" in
                SERVER_URL|UPLOAD_ENDPOINT|DOWNLOAD_ENDPOINT|DELETE_ENDPOINT|BATCH_UPLOAD_ENDPOINT|CHECK_MD5|VERBOSE|TIMEOUT)
                    # 更新配置变量
                    eval "$key=\"$value\""
                    # 保存配置
                    save_config
                    echo -e "${GREEN}配置已更新: $key=$value${NC}"
                    ;;
                *)
                    echo -e "${RED}错误: 无效的配置键 '$key'${NC}" >&2
                    echo -e "${YELLOW}可用配置项: SERVER_URL, UPLOAD_ENDPOINT, DOWNLOAD_ENDPOINT, DELETE_ENDPOINT, CHECK_MD5, VERBOSE, TIMEOUT${NC}"
                    return 1
                    ;;
            esac
            ;;
        *)
            echo -e "${RED}用法: $0 config <show|set>${NC}" >&2
            echo "  show - 显示当前配置"
            echo "  set <键> <值> - 设置配置值"
            return 1
            ;;
    esac
}

# 主函数
main() {
    # 加载配置
    load_config
    
    # 如果没有参数，显示帮助
    if [ $# -eq 0 ]; then
        show_help
        exit 0
    fi
    
    # 解析命令
    local command="$1"
    shift
    
    # 解析选项
    local files=()
    local output=""
    local check_md5="$CHECK_MD5"
    local verbose="$VERBOSE"
    local server_url="$SERVER_URL"
    
    while [ $# -gt 0 ]; do
        case "$1" in
            -s|--server)
                server_url="$2"
                shift 2
                ;;
            -o|--output)
                output="$2"
                shift 2
                ;;
            -m|--no-md5)
                check_md5="false"
                shift
                ;;
            -v|--verbose)
                verbose="true"
                shift
                ;;
            -h|--help)
                show_help
                exit 0
                ;;
            -*)
                echo -e "${RED}错误: 未知选项 $1${NC}" >&2
                show_help
                exit 1
                ;;
            *)
                files+=("$1")
                shift
                ;;
        esac
    done
    
    # 根据命令执行对应操作
    case "$command" in
        upload)
            if [ ${#files[@]} -eq 0 ]; then
                echo -e "${RED}错误: 请指定要上传的文件${NC}" >&2
                show_help
                exit 1
            fi
            
            # 设置详细模式
            VERBOSE="$verbose"
            
            if [ ${#files[@]} -eq 1 ]; then
                # 单个文件上传
                local upload_url="$server_url$UPLOAD_ENDPOINT"
                upload_file "${files[0]}" "$upload_url" "$check_md5"
            else
                # 批量上传
                local batch_url="$server_url$BATCH_UPLOAD_ENDPOINT"
                batch_upload "${files[@]}" "$batch_url"
            fi
            ;;
            
        download)
            if [ ${#files[@]} -lt 1 ]; then
                echo -e "${RED}错误: 请指定要下载的文件名${NC}" >&2
                show_help
                exit 1
            fi
            
            local filename="${files[0]}"
            if [ -z "$output" ] && [ ${#files[@]} -gt 1 ]; then
                output="${files[1]}"
            fi
            
            # 设置详细模式
            VERBOSE="$verbose"
            
            local download_url="$server_url$DOWNLOAD_ENDPOINT"
            download_file "$filename" "$output" "$download_url"
            ;;

        delete)
            if [ ${#files[@]} -ne 1 ]; then
                echo -e "${RED}错误: 删除命令需要指定一个文件名${NC}" >&2
                show_help
                exit 1
            fi
            
            local delete_url="$server_url$DELETE_ENDPOINT"
            delete_file "${files[0]}" "$delete_url"
            ;;
            
        list)
            list_files "$server_url"
            ;;
            
        config)
            manage_config "${files[@]}"
            ;;
            
        version)
            show_version
            ;;
            
        help)
            show_help
            ;;
            
        *)
            echo -e "${RED}错误: 未知命令 '$command'${NC}" >&2
            show_help
            exit 1
            ;;
    esac
}

# 如果脚本被直接执行，则调用主函数
if [[ "${BASH_SOURCE[0]}" = "${0}" ]]; then
    main "$@"
fi