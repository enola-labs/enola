require_relative "lib"
class Concrete < Base
  def initialize
    super
    @repo = Repo.new
  end
  def step
    @repo.save
  end
  def handle
    common
    @helper.work
  end
end
