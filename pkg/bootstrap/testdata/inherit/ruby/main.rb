require_relative "lib/concrete"
def main
  c = Concrete.new
  c.run
  c.handle
end
main if __FILE__ == $0
