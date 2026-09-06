/*
 * QDC507 v8 research shim.
 *
 * This is deliberately not a modem client.  It interposes only the vendor
 * qmi_client_send_msg_sync call for a WMS Raw Send-shaped request, records
 * the C structure passed by qmi_simple_ril_test, and returns an error before
 * the request can reach the modem.  For this offline capture all QMI sends
 * are rejected, so modem initialization cannot issue any side effect either.
 */

extern long write(int, const void *, unsigned long);

typedef unsigned int Elf32_Addr;
typedef unsigned int Elf32_Word;
typedef int Elf32_Sword;
typedef unsigned short Elf32_Half;

typedef struct {
    Elf32_Word p_type;
    Elf32_Addr p_offset;
    Elf32_Addr p_vaddr;
    Elf32_Addr p_paddr;
    Elf32_Word p_filesz;
    Elf32_Word p_memsz;
    Elf32_Word p_flags;
    Elf32_Word p_align;
} Elf32_Phdr;

typedef struct {
    Elf32_Sword d_tag;
    union {
        Elf32_Word d_val;
        Elf32_Addr d_ptr;
    } d_un;
} Elf32_Dyn;

typedef struct {
    Elf32_Word st_name;
    Elf32_Addr st_value;
    Elf32_Word st_size;
    unsigned char st_info;
    unsigned char st_other;
    Elf32_Half st_shndx;
} Elf32_Sym;

typedef struct {
    unsigned char e_ident[16];
    Elf32_Half e_type;
    Elf32_Half e_machine;
    Elf32_Word e_version;
    Elf32_Addr e_entry;
    Elf32_Word e_phoff;
    Elf32_Word e_shoff;
    Elf32_Word e_flags;
    Elf32_Half e_ehsize;
    Elf32_Half e_phentsize;
    Elf32_Half e_phnum;
    Elf32_Half e_shentsize;
    Elf32_Half e_shnum;
    Elf32_Half e_shstrndx;
} Elf32_Ehdr;

extern int open(const char *, int, ...);
extern int read(int, void *, unsigned long);
extern int close(int);

enum {
    PT_DYNAMIC = 2,
    DT_NULL = 0,
    DT_HASH = 4,
    DT_STRTAB = 5,
    DT_SYMTAB = 6,
    DT_SYMENT = 11,
    DT_GNU_HASH = 0x6ffffef5,
};

typedef int (*qmi_send_msg_sync_fn)(void *, unsigned short, void *, unsigned,
                                    void *, unsigned, unsigned);

static qmi_send_msg_sync_fn real_send;

int qmi_client_send_msg_sync(void *, unsigned short, void *, unsigned,
                             void *, unsigned, unsigned);
static void emit(const char *value);

static unsigned long text_len(const char *value) {
    unsigned long length = 0;
    while (value[length] != '\0') {
        length++;
    }
    return length;
}

static int has_text(const char *value, const char *needle) {
    unsigned long value_len = text_len(value);
    unsigned long needle_len = text_len(needle);
    unsigned long i;
    if (needle_len == 0 || needle_len > value_len) {
        return 0;
    }
    for (i = 0; i + needle_len <= value_len; i++) {
        unsigned long j;
        for (j = 0; j < needle_len && value[i + j] == needle[j]; j++) {
        }
        if (j == needle_len) {
            return 1;
        }
    }
    return 0;
}

static void *dynamic_pointer(Elf32_Addr base, Elf32_Addr value) {
    return (void *)(value >= base ? value : base + value);
}

static int equal_text(const char *value, const char *expected, unsigned long length) {
    unsigned long i;
    for (i = 0; i < length; i++) {
        if (value[i] != expected[i]) {
            return 0;
        }
    }
    return value[length] == '\0';
}

static Elf32_Addr find_qmi_base(void) {
    char maps[65536];
    int fd = open("/proc/self/maps", 0);
    int count;
    int i = 0;
    if (fd < 0) {
        return 0;
    }
    count = read(fd, maps, sizeof(maps) - 1);
    (void)close(fd);
    if (count <= 0) {
        return 0;
    }
    maps[count] = '\0';
    while (i < count) {
        int line_start = i;
        Elf32_Addr start = 0;
        Elf32_Addr offset = 0;
        while (i < count && maps[i] != '-' && maps[i] != '\n') {
            unsigned value;
            if (maps[i] >= '0' && maps[i] <= '9') {
                value = (unsigned)(maps[i] - '0');
            } else if (maps[i] >= 'a' && maps[i] <= 'f') {
                value = (unsigned)(maps[i] - 'a' + 10);
            } else if (maps[i] >= 'A' && maps[i] <= 'F') {
                value = (unsigned)(maps[i] - 'A' + 10);
            } else {
                break;
            }
            start = (start << 4) | value;
            i++;
        }
        if (i < count && maps[i] == '-') {
            while (i < count && maps[i] != ' ') {
                i++;
            }
            while (i < count && maps[i] == ' ') {
                i++;
            }
            while (i < count && maps[i] != ' ') {
                unsigned value;
                if (maps[i] >= '0' && maps[i] <= '9') {
                    value = (unsigned)(maps[i] - '0');
                } else if (maps[i] >= 'a' && maps[i] <= 'f') {
                    value = (unsigned)(maps[i] - 'a' + 10);
                } else if (maps[i] >= 'A' && maps[i] <= 'F') {
                    value = (unsigned)(maps[i] - 'A' + 10);
                } else {
                    break;
                }
                offset = (offset << 4) | value;
                i++;
            }
            while (i < count && maps[i] != ' ') {
                i++;
            }
            if (offset == 0 && has_text(maps + line_start, "libqmi_cci")) {
                emit("[CELLBRIDGE_QMI_CAPTURE] resolver_base_found\n");
                return start;
            }
        }
        while (i < count && maps[i] != '\n') {
            i++;
        }
        i++;
    }
    return 0;
}

static qmi_send_msg_sync_fn find_qmi_send(void) {
    Elf32_Addr base = find_qmi_base();
    const Elf32_Ehdr *header;
    const Elf32_Phdr *program_headers;
    const Elf32_Dyn *dynamic = 0;
    const Elf32_Sym *symbols = 0;
    const char *strings = 0;
    const Elf32_Word *hash = 0;
    const Elf32_Word *gnu_hash = 0;
    Elf32_Word symbol_count = 0;
    Elf32_Word symbol_entry_size = sizeof(Elf32_Sym);
    Elf32_Word i;

    if (base == 0) {
        return 0;
    }
    header = (const Elf32_Ehdr *)base;
    program_headers = (const Elf32_Phdr *)(base + header->e_phoff);
    for (i = 0; i < header->e_phnum; i++) {
        if (program_headers[i].p_type == PT_DYNAMIC) {
            dynamic = (const Elf32_Dyn *)(base + program_headers[i].p_vaddr);
            break;
        }
    }
    if (dynamic == 0) {
        return 0;
    }
    for (; dynamic->d_tag != DT_NULL; dynamic++) {
        if (dynamic->d_tag == DT_SYMTAB) {
            symbols = (const Elf32_Sym *)dynamic_pointer(base, dynamic->d_un.d_ptr);
        } else if (dynamic->d_tag == DT_STRTAB) {
            strings = (const char *)dynamic_pointer(base, dynamic->d_un.d_ptr);
        } else if (dynamic->d_tag == DT_HASH) {
            hash = (const Elf32_Word *)dynamic_pointer(base, dynamic->d_un.d_ptr);
            symbol_count = hash[1];
        } else if (dynamic->d_tag == DT_GNU_HASH) {
            gnu_hash = (const Elf32_Word *)dynamic_pointer(base, dynamic->d_un.d_ptr);
        } else if (dynamic->d_tag == DT_SYMENT && dynamic->d_un.d_val != 0) {
            symbol_entry_size = dynamic->d_un.d_val;
        }
    }
    if (symbol_count == 0 && gnu_hash != 0) {
        Elf32_Word bucket_count = gnu_hash[0];
        Elf32_Word symbol_offset = gnu_hash[1];
        Elf32_Word bloom_count = gnu_hash[2];
        const Elf32_Word *buckets = gnu_hash + 4 + bloom_count * 2;
        const Elf32_Word *chains = buckets + bucket_count;
        Elf32_Word bucket;
        for (bucket = 0; bucket < bucket_count; bucket++) {
            Elf32_Word index = buckets[bucket];
            if (index < symbol_offset) {
                continue;
            }
            for (;;) {
                if (index + 1 > symbol_count) {
                    symbol_count = index + 1;
                }
                if ((chains[index - symbol_offset] & 1) != 0) {
                    break;
                }
                index++;
            }
        }
    }
    if (symbols == 0 || strings == 0 || symbol_count == 0 || symbol_entry_size < sizeof(Elf32_Sym)) {
        return 0;
    }
    for (i = 0; i < symbol_count; i++) {
        const Elf32_Sym *symbol = (const Elf32_Sym *)((const char *)symbols + (unsigned long)i * symbol_entry_size);
        if (symbol->st_name == 0 || !equal_text(strings + symbol->st_name, "qmi_client_send_msg_sync", 24)) {
            continue;
        }
        if (symbol->st_value == 0) {
            continue;
        }
        return (qmi_send_msg_sync_fn)(base + symbol->st_value);
    }
    return 0;
}

static qmi_send_msg_sync_fn original_qmi_send(void) {
    if (real_send == 0) {
        real_send = find_qmi_send();
    }
    return real_send;
}

static void emit(const char *value) {
    (void)write(1, value, text_len(value));
}

static char hex_digit(unsigned value) {
    return value < 10 ? (char)('0' + value) : (char)('A' + value - 10);
}

static void emit_hex(const unsigned char *value, unsigned length) {
    char line[4096];
    unsigned pos = 0;
    unsigned i;
    for (i = 0; i < length && pos + 3 < sizeof(line); i++) {
        line[pos++] = hex_digit(value[i] >> 4);
        line[pos++] = hex_digit(value[i] & 0x0f);
        line[pos++] = (i + 1 == length) ? '\n' : ' ';
    }
    (void)write(1, line, pos);
}

__attribute__((visibility("default")))
int qmi_client_send_msg_sync(void *client, unsigned short message_id,
                             void *request, unsigned request_length,
                             void *response, unsigned response_length,
                             unsigned timeout_ms) {
    emit("[CELLBRIDGE_QMI_CAPTURE] qmi_call\n");
    /* WMS Raw Send is 0x0020 and the QDC507 WMS request C struct is 0x134. */
    if (message_id == 0x0020 && request_length == 0x134 && request != 0) {
        const unsigned char *bytes = (const unsigned char *)request;
        emit("[CELLBRIDGE_QMI_CAPTURE] message=0x0020 length=0x134\n");
        emit("[CELLBRIDGE_QMI_CAPTURE] struct=");
        emit_hex(bytes, request_length);
        emit("[CELLBRIDGE_QMI_CAPTURE] format=");
        emit_hex(bytes, 1);
        emit("[CELLBRIDGE_QMI_CAPTURE] raw_length_le=");
        emit_hex(bytes + 4, 4);
        emit("[CELLBRIDGE_QMI_CAPTURE] raw_prefix=");
        emit_hex(bytes + 8, request_length - 8 > 64 ? 64 : request_length - 8);
        emit("[CELLBRIDGE_QMI_CAPTURE] modem_send=blocked\n");
        return 1;
    }

    {
        qmi_send_msg_sync_fn forward;
        emit("[CELLBRIDGE_QMI_CAPTURE] resolving_original\n");
        forward = original_qmi_send();
        if (forward == 0) {
            emit("[CELLBRIDGE_QMI_CAPTURE] original_not_found\n");
            return 1;
        }
        emit("[CELLBRIDGE_QMI_CAPTURE] forwarding\n");
        return forward(client, message_id, request, request_length, response,
                       response_length, timeout_ms);
    }
}
