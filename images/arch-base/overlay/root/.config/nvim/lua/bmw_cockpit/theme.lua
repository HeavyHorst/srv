local M = {}

local palettes = {
  ["bmw-orange-dark"] = {
    appearance = "dark",
    colors_name = "bootstrap_bmw_heritage_amber_dark",
    bg = "#181B1F",
    bg_alt = "#1A1F26",
    fg = "#FFBD82",
    fg_muted = "#A3765D",
    selection = "#2A313A",
    cursor = "#FF7A1A",
    primary = "#FF6A00",
    secondary = "#FF8F3D",
    dim = "#C45100",
    ansi = {
      "#1A1F26",
      "#D36259",
      "#FF6A00",
      "#FFB347",
      "#FF8F3D",
      "#D97221",
      "#C45100",
      "#FFBD82",
      "#4B5563",
      "#F07A6E",
      "#FF8F3D",
      "#FFB347",
      "#FF8F3D",
      "#FF9F5A",
      "#FFB347",
      "#FFF0E0",
    },
  },
  ["bmw-m-light"] = {
    appearance = "light",
    colors_name = "bootstrap_bmw_m_light",
    bg = "#F2F4F7",
    bg_alt = "#E5EAF0",
    fg = "#1F252E",
    fg_muted = "#5E6773",
    selection = "#D6DEE8",
    cursor = "#B94700",
    primary = "#CC5400",
    secondary = "#1C69D4",
    dim = "#E86A17",
    ansi = {
      "#2A313B",
      "#B3362A",
      "#2E7D32",
      "#8A5A00",
      "#1C69D4",
      "#8B4FAF",
      "#006A78",
      "#5E6773",
      "#8591A1",
      "#D14A3C",
      "#3E8E41",
      "#A66D00",
      "#3C81E0",
      "#A165C4",
      "#158292",
      "#1F252E",
    },
  },
}

local function hi(group, opts)
  vim.api.nvim_set_hl(0, group, opts)
end

local function link(from, to)
  hi(from, { link = to })
end

local function set_terminal_palette(p)
  for idx, color in ipairs(p.ansi) do
    vim.g["terminal_color_" .. (idx - 1)] = color
  end
end

function M.apply(theme_name)
  local p = palettes[theme_name]
  if not p then
    error("unknown BMW LazyVim theme: " .. tostring(theme_name))
  end

  vim.o.termguicolors = true
  vim.o.background = p.appearance
  set_terminal_palette(p)

  local none = "NONE"

  hi("Normal", { fg = p.fg, bg = p.bg })
  hi("NormalNC", { fg = p.fg, bg = p.bg })
  hi("NormalFloat", { fg = p.fg, bg = p.bg_alt })
  hi("FloatBorder", { fg = p.dim, bg = p.bg_alt })
  hi("FloatTitle", { fg = p.primary, bg = p.bg_alt, bold = true })
  hi("ColorColumn", { bg = p.bg_alt })
  hi("Conceal", { fg = p.fg_muted })
  hi("Cursor", { fg = p.bg, bg = p.cursor })
  hi("CursorColumn", { bg = p.bg_alt })
  hi("CursorLine", { bg = p.bg_alt })
  hi("CursorLineNr", { fg = p.primary, bg = p.bg_alt, bold = true })
  hi("Directory", { fg = p.secondary, bold = true })
  hi("EndOfBuffer", { fg = p.bg_alt, bg = p.bg })
  hi("ErrorMsg", { fg = p.ansi[2], bg = p.bg, bold = true })
  hi("FoldColumn", { fg = p.fg_muted, bg = p.bg })
  hi("Folded", { fg = p.fg_muted, bg = p.bg_alt })
  hi("IncSearch", { fg = p.bg, bg = p.primary, bold = true })
  hi("LineNr", { fg = p.ansi[9], bg = p.bg })
  hi("MatchParen", { fg = p.primary, bg = p.selection, bold = true })
  hi("MoreMsg", { fg = p.secondary, bold = true })
  hi("NonText", { fg = p.fg_muted })
  hi("Pmenu", { fg = p.fg, bg = p.bg_alt })
  hi("PmenuSel", { fg = p.bg, bg = p.primary, bold = true })
  hi("PmenuSbar", { bg = p.selection })
  hi("PmenuThumb", { bg = p.dim })
  hi("Question", { fg = p.secondary, bold = true })
  hi("Search", { fg = p.bg, bg = p.ansi[4] })
  hi("SignColumn", { fg = p.fg_muted, bg = p.bg })
  hi("SpecialKey", { fg = p.fg_muted })
  hi("StatusLine", { fg = p.fg, bg = p.bg_alt, bold = true })
  hi("StatusLineNC", { fg = p.fg_muted, bg = p.bg_alt })
  hi("TabLine", { fg = p.fg_muted, bg = p.bg_alt })
  hi("TabLineFill", { fg = p.fg_muted, bg = p.bg_alt })
  hi("TabLineSel", { fg = p.bg, bg = p.primary, bold = true })
  hi("Title", { fg = p.primary, bold = true })
  hi("Visual", { bg = p.selection })
  hi("WarningMsg", { fg = p.ansi[4], bold = true })
  hi("Whitespace", { fg = p.fg_muted })
  hi("WildMenu", { fg = p.bg, bg = p.secondary, bold = true })
  hi("WinBar", { fg = p.fg, bg = p.bg_alt })
  hi("WinBarNC", { fg = p.fg_muted, bg = p.bg_alt })
  hi("WinSeparator", { fg = p.selection, bg = none })

  hi("Comment", { fg = p.fg_muted, italic = true })
  hi("Constant", { fg = p.ansi[16] })
  hi("String", { fg = p.ansi[4] })
  hi("Character", { fg = p.ansi[4] })
  hi("Number", { fg = p.ansi[14] })
  hi("Boolean", { fg = p.ansi[14], bold = true })
  hi("Float", { fg = p.ansi[14] })
  hi("Identifier", { fg = p.fg })
  hi("Function", { fg = p.secondary, bold = true })
  hi("Statement", { fg = p.primary, bold = true })
  hi("Conditional", { fg = p.primary, bold = true })
  hi("Repeat", { fg = p.primary, bold = true })
  hi("Label", { fg = p.primary })
  hi("Operator", { fg = p.fg })
  hi("Keyword", { fg = p.primary, bold = true })
  hi("Exception", { fg = p.ansi[2], bold = true })
  hi("PreProc", { fg = p.secondary })
  hi("Include", { fg = p.secondary })
  hi("Define", { fg = p.secondary })
  hi("Macro", { fg = p.secondary })
  hi("PreCondit", { fg = p.secondary })
  hi("Type", { fg = p.ansi[13], bold = true })
  hi("StorageClass", { fg = p.primary })
  hi("Structure", { fg = p.ansi[13] })
  hi("Typedef", { fg = p.ansi[13] })
  hi("Special", { fg = p.primary })
  hi("SpecialComment", { fg = p.fg_muted, italic = true })
  hi("Underlined", { fg = p.secondary, underline = true })
  hi("Todo", { fg = p.bg, bg = p.ansi[4], bold = true })
  hi("Delimiter", { fg = p.fg })

  hi("DiffAdd", { fg = p.ansi[3], bg = p.bg_alt })
  hi("DiffChange", { fg = p.ansi[5], bg = p.bg_alt })
  hi("DiffDelete", { fg = p.ansi[2], bg = p.bg_alt })
  hi("DiffText", { fg = p.bg, bg = p.primary, bold = true })

  hi("DiagnosticError", { fg = p.ansi[2] })
  hi("DiagnosticWarn", { fg = p.ansi[4] })
  hi("DiagnosticInfo", { fg = p.secondary })
  hi("DiagnosticHint", { fg = p.fg_muted })
  hi("DiagnosticOk", { fg = p.ansi[3] })
  hi("DiagnosticVirtualTextError", { fg = p.ansi[2], bg = p.bg_alt })
  hi("DiagnosticVirtualTextWarn", { fg = p.ansi[4], bg = p.bg_alt })
  hi("DiagnosticVirtualTextInfo", { fg = p.secondary, bg = p.bg_alt })
  hi("DiagnosticVirtualTextHint", { fg = p.fg_muted, bg = p.bg_alt })
  hi("DiagnosticUnderlineError", { undercurl = true, sp = p.ansi[2] })
  hi("DiagnosticUnderlineWarn", { undercurl = true, sp = p.ansi[4] })
  hi("DiagnosticUnderlineInfo", { undercurl = true, sp = p.secondary })
  hi("DiagnosticUnderlineHint", { undercurl = true, sp = p.fg_muted })

  hi("GitSignsAdd", { fg = p.ansi[3] })
  hi("GitSignsChange", { fg = p.secondary })
  hi("GitSignsDelete", { fg = p.ansi[2] })

  hi("TelescopeNormal", { fg = p.fg, bg = p.bg_alt })
  hi("TelescopeBorder", { fg = p.dim, bg = p.bg_alt })
  hi("TelescopeTitle", { fg = p.primary, bg = p.bg_alt, bold = true })
  hi("TelescopeSelection", { fg = p.fg, bg = p.selection })
  hi("TelescopePromptNormal", { fg = p.fg, bg = p.bg_alt })
  hi("TelescopePromptBorder", { fg = p.primary, bg = p.bg_alt })
  hi("TelescopeResultsNormal", { fg = p.fg, bg = p.bg_alt })
  hi("TelescopeResultsBorder", { fg = p.dim, bg = p.bg_alt })
  hi("TelescopePreviewNormal", { fg = p.fg, bg = p.bg_alt })
  hi("TelescopePreviewBorder", { fg = p.dim, bg = p.bg_alt })

  hi("LazyNormal", { fg = p.fg, bg = p.bg })
  hi("MasonNormal", { fg = p.fg, bg = p.bg })
  hi("WhichKey", { fg = p.primary, bold = true })
  hi("WhichKeyGroup", { fg = p.secondary })
  hi("WhichKeyDesc", { fg = p.fg })
  hi("WhichKeySeparator", { fg = p.fg_muted })

  link("@comment", "Comment")
  link("@comment.documentation", "SpecialComment")
  link("@keyword", "Keyword")
  link("@keyword.function", "Keyword")
  link("@keyword.return", "Keyword")
  link("@conditional", "Conditional")
  link("@repeat", "Repeat")
  link("@string", "String")
  link("@string.escape", "Special")
  link("@character", "Character")
  link("@number", "Number")
  link("@boolean", "Boolean")
  link("@constant", "Constant")
  link("@constant.builtin", "Constant")
  link("@constructor", "Function")
  link("@function", "Function")
  link("@function.builtin", "Function")
  link("@function.call", "Function")
  link("@method", "Function")
  link("@type", "Type")
  link("@type.builtin", "Type")
  link("@property", "Identifier")
  link("@field", "Identifier")
  link("@variable", "Identifier")
  link("@variable.builtin", "Constant")
  link("@variable.parameter", "Identifier")
  link("@module", "Type")
  link("@namespace", "Type")
  link("@operator", "Operator")
  link("@punctuation", "Delimiter")
  link("@punctuation.bracket", "Delimiter")
  link("@punctuation.delimiter", "Delimiter")

  vim.g.colors_name = p.colors_name
end

return M
